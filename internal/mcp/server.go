// Package mcp wires the portolan tools (internal/tools) onto the
// Model Context Protocol SDK: it builds an *mcp.Server exposing the three
// tools over stdio, and runs a project-keyed control socket used by the
// Phase 1 staleness barrier (see control.go).
package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/skflowne/portolan/internal/tools"
)

// serverName/serverVersion identify this daemon to MCP clients.
const (
	serverName    = "portolan"
	serverVersion = "0.0.1"
)

// NewServer builds an MCP server with the three code-graph tools
// (find_definition, find_references, get_outline) registered against t. The
// returned server is not yet connected to any transport; call RunStdio (or
// srv.Run/Connect directly) to serve it.
func NewServer(t *tools.Tools) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{
		Name:    serverName,
		Version: serverVersion,
	}, nil)

	sdk.AddTool(srv, &sdk.Tool{
		Name: "find_definition",
		Description: "Navigate the code graph (via the language server) to find where a " +
			"symbol is defined. Given a file and a symbol name (function, method, type, " +
			"variable, etc.), resolves the symbol's position by name in that file's outline " +
			"and returns the location(s) of its definition, each with the file path and a " +
			"precise range. Use `line` to disambiguate when the same name appears more than " +
			"once in the file. Every result carries a freshness stamp (generation/stale) so " +
			"the caller knows whether the graph was rebuilt since the file was last edited. " +
			"An empty result (found:false) means the symbol name did not resolve or has no " +
			"definition on record -- it is not an error.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in tools.FindDefinitionInput) (*sdk.CallToolResult, tools.FindDefinitionOutput, error) {
		out, err := t.FindDefinition(ctx, in)
		return nil, out, err
	})

	sdk.AddTool(srv, &sdk.Tool{
		Name: "find_references",
		Description: "Navigate the code graph (via the language server) to find every " +
			"reference to a symbol, including its declaration. Given a file and a symbol " +
			"name, resolves the symbol's position by name in that file's outline and returns " +
			"every location where it is used across the project. Use `line` to disambiguate " +
			"when the same name appears more than once in the file. Results are capped (see " +
			"`truncated`) and every result carries a freshness stamp (generation/stale). An " +
			"empty result (found:false) means the symbol name did not resolve or has no " +
			"references on record -- it is not an error.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in tools.FindReferencesInput) (*sdk.CallToolResult, tools.FindReferencesOutput, error) {
		out, err := t.FindReferences(ctx, in)
		return nil, out, err
	})

	sdk.AddTool(srv, &sdk.Tool{
		Name: "get_outline",
		Description: "Navigate the code graph (via the language server) to get a file's " +
			"structural outline: every top-level and nested symbol (classes, functions, " +
			"methods, fields, etc.) with its kind, declaration signature, and precise range, " +
			"flattened into a depth-tagged list (depth 0 = top-level; a child immediately " +
			"follows its parent in the list). Prefer this over reading a whole file when you " +
			"only need to know what's in it and where. Results are capped (see `truncated`) " +
			"and carry a freshness stamp (generation/stale).",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in tools.GetOutlineInput) (*sdk.CallToolResult, tools.GetOutlineOutput, error) {
		out, err := t.GetOutline(ctx, in)
		return nil, out, err
	})

	return srv
}

// RunStdio serves srv over stdin/stdout using newline-delimited JSON framing,
// blocking until the client disconnects or ctx is cancelled.
func RunStdio(ctx context.Context, srv *sdk.Server) error {
	return srv.Run(ctx, &sdk.StdioTransport{})
}
