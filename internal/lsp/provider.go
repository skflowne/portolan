// Package lsp implements core.LanguageProvider against tsgo --lsp -stdio, the
// native-preview TypeScript language server. It adapts LSP navigation and
// semantic presentation data into core types without graph-building.
package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sync"
	"time"

	"github.com/skflowne/portolan/internal/core"
	"github.com/skflowne/portolan/internal/pathnorm"
)

const initializeTimeout = 20 * time.Second

// Provider is a core.LanguageProvider backed by a tsgo --lsp -stdio
// subprocess. All exported methods are safe for concurrent use.
type Provider struct {
	transport *transport

	openMu    sync.Mutex
	openFiles map[string]*openTransition
	readFile  func(context.Context, string) ([]byte, error)
}

var _ core.LanguageProvider = (*Provider)(nil)

// New starts tsgo and completes the LSP initialize handshake.
func New(cfg core.Config) (*Provider, error) {
	_, rootURI, rootName, err := workspaceIdentity(cfg.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("lsp: project root %q: %w", cfg.ProjectRoot, err)
	}

	tsgoPath := cfg.TsgoPath
	if tsgoPath == "" {
		tsgoPath = "tsgo"
	}
	cmd := exec.Command(tsgoPath, "--lsp", "-stdio")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: starting %s --lsp -stdio: %w", tsgoPath, err)
	}

	stderrBuf := newStderrBuffer()
	connection := newTransport(transportConfig{
		input:       stdin,
		output:      stdout,
		stderr:      stderrBuf,
		killProcess: cmd.Process.Kill,
		waitProcess: cmd.Wait,
	})
	p := &Provider{
		transport: connection,
		openFiles: make(map[string]*openTransition),
		readFile: func(_ context.Context, path string) ([]byte, error) {
			return os.ReadFile(path)
		},
	}
	go stderrBuf.drain(stderr)
	go connection.readLoop()

	if err := initializeProvider(rootURI, rootName, connection.call, connection.notify); err != nil {
		connection.abortAndWait(errProviderClosed)
		return nil, err
	}
	return p, nil
}

func workspaceIdentity(projectRoot string) (canonicalPath, uri, name string, err error) {
	canonicalPath, err = pathnorm.Canonicalize(projectRoot)
	if err != nil {
		return "", "", "", err
	}
	uri, err = pathnorm.PathToURI(canonicalPath)
	if err != nil {
		return "", "", "", err
	}
	return canonicalPath, uri, path.Base(canonicalPath), nil
}

type requestFunc func(context.Context, string, any) (json.RawMessage, error)
type notificationFunc func(context.Context, string, any) error

func initializeProvider(rootURI, rootName string, request requestFunc, notify notificationFunc) error {
	ctx, cancel := context.WithTimeout(context.Background(), initializeTimeout)
	defer cancel()

	params := initializeParams{
		ProcessID: os.Getpid(),
		RootURI:   rootURI,
		Capabilities: clientCapabilities{
			TextDocument: &textDocumentClientCapabilities{
				DocumentSymbol: &documentSymbolClientCapabilities{
					HierarchicalDocumentSymbolSupport: true,
				},
				Hover: &hoverClientCapabilities{ContentFormat: []string{"markdown"}},
			},
		},
		WorkspaceFolders: []workspaceFolder{{URI: rootURI, Name: rootName}},
	}
	if _, err := request(ctx, "initialize", params); err != nil {
		return fmt.Errorf("lsp: initialize handshake: %w", err)
	}
	if err := notify(ctx, "initialized", struct{}{}); err != nil {
		return fmt.Errorf("lsp: initialized notification: %w", err)
	}
	return nil
}

// prepareOpen validates file identity and sends one canonical
// textDocument/didOpen before returning its URI.
func (p *Provider) prepareOpen(ctx context.Context, file string) (canonicalFile, uri string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	canonicalFile, err = pathnorm.Canonicalize(file)
	if err != nil {
		return "", "", fmt.Errorf("lsp: canonicalizing path %q: %w", file, err)
	}
	uri, err = pathnorm.PathToURI(canonicalFile)
	if err != nil {
		return "", "", fmt.Errorf("lsp: encoding path %q: %w", canonicalFile, err)
	}
	if err := p.ensureOpen(ctx, canonicalFile); err != nil {
		return "", "", err
	}
	return canonicalFile, uri, nil
}

type openTransition struct {
	done      chan struct{}
	source    string
	err       error
	retryable bool
}

func (p *Provider) readFileContext(ctx context.Context, path string) (string, error) {
	interruption := finiteWorkInterruption{
		done:  p.transport.unavailableDone(),
		cause: p.transport.unavailableError,
	}
	return runFiniteWork(ctx, &interruption, func() (string, error) {
		data, err := p.readFile(ctx, path)
		if err != nil {
			return "", err
		}
		return string(data), nil
	})
}

// ensureOpen elects one opener per file while unrelated files proceed.
func (p *Provider) ensureOpen(ctx context.Context, canonicalFile string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.openMu.Lock()
		transition, exists := p.openFiles[canonicalFile]
		if !exists {
			transition = &openTransition{done: make(chan struct{})}
			p.openFiles[canonicalFile] = transition
		}
		p.openMu.Unlock()

		if exists {
			select {
			case <-transition.done:
				if transition.retryable {
					continue
				}
				return transition.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		source, err, retryable := p.openFile(ctx, canonicalFile)
		p.openMu.Lock()
		transition.source = source
		transition.err = err
		transition.retryable = retryable
		if err != nil {
			delete(p.openFiles, canonicalFile)
		}
		close(transition.done)
		p.openMu.Unlock()
		return err
	}
}

func (p *Provider) openFile(ctx context.Context, canonicalFile string) (string, error, bool) {
	uri, err := pathnorm.PathToURI(canonicalFile)
	if err != nil {
		return "", fmt.Errorf("lsp: encoding path %q: %w", canonicalFile, err), false
	}
	text, err := p.readFileContext(ctx, canonicalFile)
	if err != nil {
		return "", fmt.Errorf("lsp: reading %s: %w", canonicalFile, err), p.retryableOpenError(ctx, err)
	}
	params := didOpenParams{
		TextDocument: textDocumentItem{
			URI:        uri,
			LanguageID: languageIDForFile(canonicalFile),
			Version:    1,
			Text:       text,
		},
	}
	if err := p.transport.notify(ctx, "textDocument/didOpen", params); err != nil {
		return "", fmt.Errorf("lsp: didOpen %s: %w", canonicalFile, err), p.retryableOpenError(ctx, err)
	}
	return text, nil, false
}

func (p *Provider) openedSource(canonicalFile string) (string, bool) {
	p.openMu.Lock()
	defer p.openMu.Unlock()
	transition, ok := p.openFiles[canonicalFile]
	if !ok || transition.err != nil {
		return "", false
	}
	select {
	case <-transition.done:
		return transition.source, true
	default:
		return "", false
	}
}

func (p *Provider) retryableOpenError(ctx context.Context, err error) bool {
	ctxErr := ctx.Err()
	return ctxErr != nil && errors.Is(err, ctxErr) && p.transport.isOpen()
}

func (p *Provider) Definition(ctx context.Context, file string, pos core.Position) ([]core.Location, error) {
	_, uri, err := p.prepareOpen(ctx, file)
	if err != nil {
		return nil, err
	}
	params := textDocumentPositionParams{
		TextDocument: textDocumentIdentifier{URI: uri},
		Position:     lspPosition{Line: pos.Line, Character: pos.Character},
	}
	raw, err := p.transport.call(ctx, "textDocument/definition", params)
	if err != nil {
		return nil, err
	}
	return decodeLocations(ctx, raw)
}

func (p *Provider) References(ctx context.Context, file string, pos core.Position, includeDeclaration bool) ([]core.Location, error) {
	_, uri, err := p.prepareOpen(ctx, file)
	if err != nil {
		return nil, err
	}
	params := referenceParams{
		textDocumentPositionParams: textDocumentPositionParams{
			TextDocument: textDocumentIdentifier{URI: uri},
			Position:     lspPosition{Line: pos.Line, Character: pos.Character},
		},
		Context: referenceContext{IncludeDeclaration: includeDeclaration},
	}
	raw, err := p.transport.call(ctx, "textDocument/references", params)
	if err != nil {
		return nil, err
	}
	return decodeLocations(ctx, raw)
}

func (p *Provider) DocumentSymbols(ctx context.Context, file string) ([]core.SymbolNode, error) {
	absFile, uri, err := p.prepareOpen(ctx, file)
	if err != nil {
		return nil, err
	}
	params := documentSymbolParams{TextDocument: textDocumentIdentifier{URI: uri}}
	raw, err := p.transport.call(ctx, "textDocument/documentSymbol", params)
	if err != nil {
		return nil, err
	}
	return decodeDocumentSymbols(ctx, raw, absFile)
}
