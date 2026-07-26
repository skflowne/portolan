// Package lsp implements core.LanguageProvider against tsgo --lsp -stdio, the
// native-preview TypeScript language server. It is a thin passthrough: LSP
// requests in, core types out, with no caching or graph-building of its own
// (that lives above this package).
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
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
	// defaultRequestTimeout bounds every request/response round trip so a
	// stuck or crashed tsgo can never hang a caller indefinitely.
	defaultRequestTimeout = 5 * time.Second

	// initializeTimeout is more generous: tsgo's first initialize can involve
	// loading the TS program (tsconfig resolution, parsing), which is slower
	// than a steady-state definition/references/documentSymbol call.
	initializeTimeout = 20 * time.Second

	// shutdownTimeout bounds the best-effort shutdown handshake in Close.
	shutdownTimeout = 2 * time.Second

	// exitWait bounds how long Close waits for the subprocess to exit on its
	// own before it is killed.
	exitWait = 3 * time.Second
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

	lifecycle transportLifecycle
	closeOnce sync.Once
	closeErr  error

	openMu    sync.Mutex
	openFiles map[string]bool
	readFile  func(string) ([]byte, error)

	stderrBuf *stderrBuffer
	timeout   time.Duration
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
		openFiles: make(map[string]bool),
		stderrBuf: newStderrBuffer(),
		writeGate: newWriteGate(),
		readFile:  os.ReadFile,
		timeout:   defaultRequestTimeout,
	}

	go p.stderrBuf.drain(stderr)
	go p.readLoop()

	root, err := filepath.Abs(cfg.ProjectRoot)
	if err != nil {
		p.killAndWait()
		return nil, fmt.Errorf("lsp: resolving project root %q: %w", cfg.ProjectRoot, err)
	}
	rootURI := uriFromPath(root)

	ctx, cancel := context.WithTimeout(context.Background(), initializeTimeout)
	defer cancel()

	initParams := initializeParams{
		ProcessID: os.Getpid(),
		RootURI:   rootURI,
		Capabilities: clientCapabilities{
			TextDocument: &textDocumentClientCapabilities{
				DocumentSymbol: &documentSymbolClientCapabilities{
					HierarchicalDocumentSymbolSupport: true,
				},
			},
		},
		WorkspaceFolders: []workspaceFolder{{URI: rootURI, Name: filepath.Base(root)}},
	}

	if _, err := p.call(ctx, "initialize", initParams); err != nil {
		p.killAndWait()
		return nil, fmt.Errorf("lsp: initialize handshake: %w", err)
	}
	if err := p.notify("initialized", struct{}{}); err != nil {
		p.killAndWait()
		return nil, fmt.Errorf("lsp: initialized notification: %w", err)
	}

	return p, nil
}

// killAndWait force-terminates the subprocess; used on setup failure paths.
func (p *Provider) killAndWait() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
	}
}

// prepareOpen resolves file to an absolute path, ensures it has been sent to
// the server via textDocument/didOpen, and returns both the absolute path and
// its file:// URI.
func (p *Provider) prepareOpen(file string) (absFile, uri string, err error) {
	absFile, err = filepath.Abs(file)
	if err != nil {
		return "", "", fmt.Errorf("lsp: resolving path %q: %w", file, err)
	}
	if err := p.ensureOpen(absFile); err != nil {
		return "", "", err
	}
	return absFile, uriFromPath(absFile), nil
}

// ensureOpen sends textDocument/didOpen for absFile the first time it's seen,
// caching that it's open so repeat queries are free. It holds openMu for the
// whole check-then-open sequence so two concurrent callers for the same file
// can't both send didOpen (which the server would reject on the second one).
func (p *Provider) ensureOpen(absFile string) error {
	p.openMu.Lock()
	defer p.openMu.Unlock()

	if p.openFiles[absFile] {
		return nil
	}

	data, err := p.readFile(absFile)
	if err != nil {
		return fmt.Errorf("lsp: reading %s: %w", absFile, err)
	}

	params := didOpenParams{
		TextDocument: textDocumentItem{
			URI:        uriFromPath(absFile),
			LanguageID: languageIDForFile(absFile),
			Version:    1,
			Text:       string(data),
		},
	}
	if err := p.notify("textDocument/didOpen", params); err != nil {
		return fmt.Errorf("lsp: didOpen %s: %w", absFile, err)
	}
	p.openFiles[absFile] = true
	return nil
}

// Definition implements core.LanguageProvider.
func (p *Provider) Definition(ctx context.Context, file string, pos core.Position) ([]core.Location, error) {
	_, uri, err := p.prepareOpen(file)
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
	return decodeLocations(raw)
}

// References implements core.LanguageProvider.
func (p *Provider) References(ctx context.Context, file string, pos core.Position, includeDeclaration bool) ([]core.Location, error) {
	_, uri, err := p.prepareOpen(file)
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
	return decodeLocations(raw)
}

// DocumentSymbols implements core.LanguageProvider.
func (p *Provider) DocumentSymbols(ctx context.Context, file string) ([]core.Symbol, error) {
	absFile, uri, err := p.prepareOpen(file)
	if err != nil {
		return nil, err
	}
	params := documentSymbolParams{TextDocument: textDocumentIdentifier{URI: uri}}
	raw, err := p.call(ctx, "textDocument/documentSymbol", params)
	if err != nil {
		return nil, err
	}
	if isJSONNull(raw) {
		return nil, nil
	}

	var syms []lspDocumentSymbol
	if err := json.Unmarshal(raw, &syms); err != nil {
		return nil, fmt.Errorf("lsp: decoding documentSymbol result: %w", err)
	}
	if len(syms) == 0 {
		return nil, nil
	}

	out := make([]core.Symbol, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.toCoreSymbol(absFile))
	}
	return out, nil
}

func isJSONNull(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	trimmed := string(raw)
	return trimmed == "null"
}

// decodeLocations handles the three shapes textDocument/definition and
// textDocument/references may return: null, Location | Location[], or
// LocationLink[].
func decodeLocations(raw json.RawMessage) ([]core.Location, error) {
	if isJSONNull(raw) {
		return nil, nil
	}

	var list []rawLocation
	if err := json.Unmarshal(raw, &list); err != nil {
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
		loc, ok, err := rl.toLocation()
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, loc)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
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

func (s lspDocumentSymbol) toCoreSymbol(file string) core.Symbol {
	var children []core.Symbol
	if len(s.Children) > 0 {
		children = make([]core.Symbol, 0, len(s.Children))
		for _, c := range s.Children {
			children = append(children, c.toCoreSymbol(file))
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
	}
}
