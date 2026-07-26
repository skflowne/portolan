// Package lsp implements core.LanguageProvider against tsgo --lsp -stdio, the
// native-preview TypeScript language server. It is a thin passthrough: LSP
// requests in, core types out, with no caching or graph-building of its own
// (that lives above this package).
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skflowne/portolan/internal/core"
)

const (
	// initializeTimeout is more generous: tsgo's first initialize can involve
	// loading the TS program (tsconfig resolution, parsing), which is slower
	// than a steady-state definition/references/documentSymbol call.
	initializeTimeout = 20 * time.Second

	// The shutdown defaults bound the best-effort handshake, graceful process
	// exit, and reaping after a forced kill.
	defaultShutdownTimeout = 2 * time.Second
	defaultExitWait        = 3 * time.Second
	defaultKillWait        = time.Second
)

// Provider is a core.LanguageProvider backed by a tsgo --lsp -stdio
// subprocess. All exported methods are safe for concurrent use by multiple
// goroutines.
type Provider struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdoutR *bufio.Reader

	writeGate chan struct{}
	nextID    atomic.Int64

	lifecycle      transportLifecycle
	closeOnce      sync.Once
	closeErr       error
	abortOnce      sync.Once
	stdinCloseOnce sync.Once
	stdinCloseErr  error
	killProcess    func() error
	waitProcess    func() error

	openMu    sync.Mutex
	openFiles map[string]*openTransition
	readFile  func(context.Context, string) ([]byte, error)

	stderrBuf                *stderrBuffer
	internalWriteTimeout     time.Duration
	cancellationWriteTimeout time.Duration
	shutdownTimeout          time.Duration
	exitWait                 time.Duration
	killWait                 time.Duration
	afterFrameDispatch       func()
	observeCancellation      func(error)
}

// compile-time assertion that Provider satisfies core.LanguageProvider.
var _ core.LanguageProvider = (*Provider)(nil)

// New spawns `tsgo --lsp -stdio` (or cfg.TsgoPath if set), performs the LSP
// initialize/initialized handshake against cfg.ProjectRoot, and returns a
// ready-to-use Provider. On any failure the subprocess is killed and an error
// is returned.
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

	p := &Provider{
		cmd:       cmd,
		stdin:     stdin,
		stdoutR:   bufio.NewReader(stdout),
		lifecycle: newTransportLifecycle(),
		openFiles: make(map[string]*openTransition),
		stderrBuf: newStderrBuffer(),
		writeGate: newWriteGate(),
		readFile: func(_ context.Context, path string) ([]byte, error) {
			return os.ReadFile(path)
		},
		killProcess:              cmd.Process.Kill,
		waitProcess:              cmd.Wait,
		internalWriteTimeout:     defaultInternalWriteTimeout,
		cancellationWriteTimeout: defaultCancellationWriteTimeout,
		shutdownTimeout:          defaultShutdownTimeout,
		exitWait:                 defaultExitWait,
		killWait:                 defaultKillWait,
	}

	go p.stderrBuf.drain(stderr)
	go p.readLoop()

	root, err := filepath.Abs(cfg.ProjectRoot)
	if err != nil {
		p.killAndWait()
		return nil, fmt.Errorf("lsp: resolving project root %q: %w", cfg.ProjectRoot, err)
	}
	rootURI := uriFromPath(root)

	if err := initializeProvider(rootURI, filepath.Base(root), p.call, p.notify); err != nil {
		p.killAndWait()
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

// killAndWait force-terminates the subprocess; used on setup failure paths.
func (p *Provider) killAndWait() {
	p.abortTransport(errProviderClosed)
	p.waitForProcess(p.exitWait)
}

// prepareOpen resolves file to an absolute path, ensures it has been sent to
// the server via textDocument/didOpen, and returns both the absolute path and
// its file:// URI.
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
	}
}

// ensureOpen elects one opener per file. External work happens outside
// openMu, so first opens for unrelated files proceed independently.
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
	if err := p.notify(ctx, "textDocument/didOpen", params); err != nil {
		return fmt.Errorf("lsp: didOpen %s: %w", absFile, err), p.retryableOpenError(ctx, err)
	}
	return nil, false
}

func (p *Provider) retryableOpenError(ctx context.Context, err error) bool {
	ctxErr := ctx.Err()
	return ctxErr != nil && errors.Is(err, ctxErr) && p.lifecycle.isOpen()
}

// Definition implements core.LanguageProvider.
func (p *Provider) Definition(ctx context.Context, file string, pos core.Position) ([]core.Location, error) {
	_, uri, err := p.prepareOpen(ctx, file)
	if err != nil {
		return nil, err
	}
	params := textDocumentPositionParams{
		TextDocument: textDocumentIdentifier{URI: uri},
		Position:     lspPosition{Line: pos.Line, Character: pos.Character},
	}
	raw, err := p.call(ctx, "textDocument/definition", params)
	if err != nil {
		return nil, err
	}
	return decodeLocations(ctx, raw)
}

// References implements core.LanguageProvider.
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
	raw, err := p.call(ctx, "textDocument/references", params)
	if err != nil {
		return nil, err
	}
	return decodeLocations(ctx, raw)
}

// DocumentSymbols implements core.LanguageProvider.
func (p *Provider) DocumentSymbols(ctx context.Context, file string) ([]core.Symbol, error) {
	absFile, uri, err := p.prepareOpen(ctx, file)
	if err != nil {
		return nil, err
	}
	params := documentSymbolParams{TextDocument: textDocumentIdentifier{URI: uri}}
	raw, err := p.call(ctx, "textDocument/documentSymbol", params)
	if err != nil {
		return nil, err
	}
	return decodeDocumentSymbols(ctx, raw, absFile)
}

func isJSONNull(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	trimmed := string(raw)
	return trimmed == "null"
}

func decodeDocumentSymbols(ctx context.Context, raw json.RawMessage, file string) ([]core.Symbol, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isJSONNull(raw) {
		return nil, nil
	}
	return runContextWork(ctx, func() ([]core.Symbol, error) {
		var syms []lspDocumentSymbol
		if err := json.Unmarshal(raw, &syms); err != nil {
			return nil, fmt.Errorf("lsp: decoding documentSymbol result: %w", err)
		}
		if len(syms) == 0 {
			return nil, nil
		}

		out := make([]core.Symbol, 0, len(syms))
		for _, s := range syms {
			symbol, err := s.toCoreSymbol(ctx, file)
			if err != nil {
				return nil, err
			}
			out = append(out, symbol)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return out, nil
	})
}

// decodeLocations handles the three shapes textDocument/definition and
// textDocument/references may return: null, Location | Location[], or
// LocationLink[].
func decodeLocations(ctx context.Context, raw json.RawMessage) ([]core.Location, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isJSONNull(raw) {
		return nil, nil
	}
	return runContextWork(ctx, func() ([]core.Location, error) {
		var list []rawLocation
		if err := json.Unmarshal(raw, &list); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			var single rawLocation
			if err2 := json.Unmarshal(raw, &single); err2 != nil {
				return nil, fmt.Errorf("lsp: decoding location result: %w", err)
			}
			list = []rawLocation{single}
		}
		if len(list) == 0 {
			return nil, nil
		}

		out := make([]core.Location, 0, len(list))
		for _, rl := range list {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			loc, ok, err := rl.toLocation()
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, loc)
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	})
}

func (rl rawLocation) toLocation() (core.Location, bool, error) {
	uri := rl.URI
	rng := rl.Range
	if uri == "" && rl.TargetURI != "" {
		uri = rl.TargetURI
		if rl.TargetSelectionRange != nil {
			rng = rl.TargetSelectionRange
		} else {
			rng = rl.TargetRange
		}
	}
	if uri == "" || rng == nil {
		return core.Location{}, false, nil
	}
	path, err := pathFromURI(uri)
	if err != nil {
		return core.Location{}, false, err
	}
	return core.Location{
		File:  path,
		Range: rng.toCoreRange(),
	}, true, nil
}

func (r lspRange) toCoreRange() core.Range {
	return core.Range{
		Start: core.Position{Line: r.Start.Line, Character: r.Start.Character},
		End:   core.Position{Line: r.End.Line, Character: r.End.Character},
	}
}

func (s lspDocumentSymbol) toCoreSymbol(ctx context.Context, file string) (core.Symbol, error) {
	if err := ctx.Err(); err != nil {
		return core.Symbol{}, err
	}
	var children []core.Symbol
	if len(s.Children) > 0 {
		children = make([]core.Symbol, 0, len(s.Children))
		for _, c := range s.Children {
			child, err := c.toCoreSymbol(ctx, file)
			if err != nil {
				return core.Symbol{}, err
			}
			children = append(children, child)
		}
	}
	return core.Symbol{
		Name:     s.Name,
		Kind:     core.SymbolKind(symbolKindName(s.Kind)),
		File:     file,
		Range:    s.Range.toCoreRange(),
		SelRange: s.SelectionRange.toCoreRange(),
		Detail:   s.Detail,
		Children: children,
	}, nil
}
