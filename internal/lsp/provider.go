// Package lsp implements core.LanguageProvider against tsgo --lsp -stdio, the
// native-preview TypeScript language server. It is a thin passthrough: LSP
// requests in, core types out, with no caching or graph-building of its own.
package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/skflowne/portolan/internal/core"
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

	root, err := filepath.Abs(cfg.ProjectRoot)
	if err != nil {
		connection.abortAndWait(errProviderClosed)
		return nil, fmt.Errorf("lsp: resolving project root %q: %w", cfg.ProjectRoot, err)
	}
	rootURI := uriFromPath(root)
	if err := initializeProvider(rootURI, filepath.Base(root), connection.call, connection.notify); err != nil {
		connection.abortAndWait(errProviderClosed)
		return nil, err
	}
	return p, nil
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

// prepareOpen resolves file to an absolute path and sends one canonical
// textDocument/didOpen before returning its URI.
func (p *Provider) prepareOpen(ctx context.Context, file string) (absFile, uri string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	absFile, err = filepath.Abs(file)
	if err != nil {
		return "", "", fmt.Errorf("lsp: resolving path %q: %w", file, err)
	}
	if err := p.ensureOpen(ctx, absFile); err != nil {
		return "", "", err
	}
	return absFile, uriFromPath(absFile), nil
}

type openTransition struct {
	done      chan struct{}
	err       error
	retryable bool
}

type fileReadResult struct {
	text string
	err  error
}

func (p *Provider) readFileContext(ctx context.Context, path string) (string, error) {
	result := make(chan fileReadResult, 1)
	go func() {
		data, err := p.readFile(ctx, path)
		text := ""
		if err == nil {
			text = string(data)
		}
		result <- fileReadResult{text: text, err: err}
	}()
	select {
	case got := <-result:
		return got.text, got.err
	case <-ctx.Done():
		select {
		case got := <-result:
			return got.text, got.err
		default:
			return "", ctx.Err()
		}
	case <-p.transport.unavailableDone():
		select {
		case got := <-result:
			return got.text, got.err
		default:
			return "", p.transport.unavailableError()
		}
	}
}

// ensureOpen elects one opener per file while unrelated files proceed.
func (p *Provider) ensureOpen(ctx context.Context, absFile string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.openMu.Lock()
		transition, exists := p.openFiles[absFile]
		if !exists {
			transition = &openTransition{done: make(chan struct{})}
			p.openFiles[absFile] = transition
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

		err, retryable := p.openFile(ctx, absFile)
		p.openMu.Lock()
		transition.err = err
		transition.retryable = retryable
		if err != nil {
			delete(p.openFiles, absFile)
		}
		close(transition.done)
		p.openMu.Unlock()
		return err
	}
}

func (p *Provider) openFile(ctx context.Context, absFile string) (error, bool) {
	text, err := p.readFileContext(ctx, absFile)
	if err != nil {
		return fmt.Errorf("lsp: reading %s: %w", absFile, err), p.retryableOpenError(ctx, err)
	}
	params := didOpenParams{
		TextDocument: textDocumentItem{
			URI:        uriFromPath(absFile),
			LanguageID: languageIDForFile(absFile),
			Version:    1,
			Text:       text,
		},
	}
	if err := p.transport.notify(ctx, "textDocument/didOpen", params); err != nil {
		return fmt.Errorf("lsp: didOpen %s: %w", absFile, err), p.retryableOpenError(ctx, err)
	}
	return nil, false
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

func (p *Provider) DocumentSymbols(ctx context.Context, file string) ([]core.Symbol, error) {
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
