// Package mcp wires the portolan tools (internal/tools) onto the
// Model Context Protocol SDK: it builds an *mcp.Server exposing the three
// tools over stdio and owns the project-keyed control socket.
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
			"methods, fields, etc.) with its declaration as written in the source, enriched " +
			"with the types the language server infers. Prefer this over reading a whole " +
			"file when you only need to know what's in it and where.\n\n" +
			"The reply is compact text. Line 1 is `file <path>`; line 2 is `ranges 0-based`, " +
			"meaning every `[startLine:startCharacter-endLine:endCharacter]` that follows is " +
			"a zero-based, half-open range over the symbol's complete declaration. Then one " +
			"line per symbol, indented two spaces per nesting level, in the order the " +
			"language server reports them; a symbol with no available declaration text falls " +
			"back to its kind and name. A blank line precedes a top-level symbol that follows " +
			"a nested one. The last line is `N symbols; complete`, or " +
			"`N symbols; truncated: more symbols exist` when the result cap was reached.\n\n" +
			"A file the language server has no symbols for answers `empty: <reason>`, which " +
			"is an honest result and not a failure. An invalid path or a provider failure " +
			"answers `error: <what failed>: <cause>`. Result freshness is tracked internally " +
			"and is not part of this text.",
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in tools.GetOutlineInput) (*sdk.CallToolResult, any, error) {
		out, err := t.GetOutline(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: tools.RenderOutline(out)}},
		}, nil, nil
	})

	return srv
}

// RunStdio serves srv over stdin/stdout using newline-delimited JSON framing,
// blocking until the client disconnects or ctx is cancelled.
func RunStdio(ctx context.Context, srv *sdk.Server) error {
	return srv.Run(ctx, &sdk.StdioTransport{})
}
