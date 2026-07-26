package lsp

import "encoding/json"

// This file holds the minimal JSON-RPC / LSP wire types needed to drive
// tsgo --lsp -stdio. Only the fields we actually read or write are declared —
// this is a thin, purpose-built client, not a general LSP SDK.

// jsonrpcMessage is the shape used to decode any message coming from the
// server: a response to one of our requests, a notification, or (in theory) a
// server-initiated request.
// ID is a json.RawMessage rather than a typed int64 because JSON-RPC ids may
// be numbers or strings — tsgo, for instance, sends server-initiated
// requests (client/registerCapability) with string ids like "ts1", not just
// numeric replies to our own requests.
type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// --- outgoing request/notification envelopes ---

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcErrorResponse's ID is a raw JSON value so it can echo back whatever id
// shape the peer sent (number or string) verbatim.
type rpcErrorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   rpcError        `json:"error"`
}

type cancelParams struct {
	ID int64 `json:"id"`
}

// --- initialize ---

type initializeParams struct {
	ProcessID        int                `json:"processId"`
	RootURI          string             `json:"rootUri"`
	Capabilities     clientCapabilities `json:"capabilities"`
	WorkspaceFolders []workspaceFolder  `json:"workspaceFolders,omitempty"`
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type clientCapabilities struct {
	TextDocument *textDocumentClientCapabilities `json:"textDocument,omitempty"`
}

type textDocumentClientCapabilities struct {
	DocumentSymbol *documentSymbolClientCapabilities `json:"documentSymbol,omitempty"`
}

type documentSymbolClientCapabilities struct {
	// Requests the nested DocumentSymbol[] shape (rather than flat
	// SymbolInformation[]) from textDocument/documentSymbol.
	HierarchicalDocumentSymbolSupport bool `json:"hierarchicalDocumentSymbolSupport"`
}

// --- didOpen ---

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

// --- shared position/range/identifier types ---

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     lspPosition            `json:"position"`
}

// --- definition / references ---

type referenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

type referenceParams struct {
	textDocumentPositionParams
	Context referenceContext `json:"context"`
}

// rawLocation unifies the two shapes textDocument/definition may return:
// plain Location ({uri, range}) and LocationLink ({targetUri, targetRange,
// targetSelectionRange}). Only the fields present in the wire JSON populate;
// the rest stay zero.
type rawLocation struct {
	URI                  string    `json:"uri"`
	Range                *lspRange `json:"range"`
	TargetURI            string    `json:"targetUri"`
	TargetRange          *lspRange `json:"targetRange"`
	TargetSelectionRange *lspRange `json:"targetSelectionRange"`
}

// --- documentSymbol ---

type documentSymbolParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Detail         string              `json:"detail,omitempty"`
	Kind           int                 `json:"kind"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children,omitempty"`
}

// symbolKindNames maps the LSP SymbolKind enum (1..26) to the lower-cased
// names core.Symbol.Kind expects.
var symbolKindNames = map[int]string{
	1:  "file",
	2:  "module",
	3:  "namespace",
	4:  "package",
	5:  "class",
	6:  "method",
	7:  "property",
	8:  "field",
	9:  "constructor",
	10: "enum",
	11: "interface",
	12: "function",
	13: "variable",
	14: "constant",
	15: "string",
	16: "number",
	17: "boolean",
	18: "array",
	19: "object",
	20: "key",
	21: "null",
	22: "enummember",
	23: "struct",
	24: "event",
	25: "operator",
	26: "typeparameter",
}

func symbolKindName(k int) string {
	if name, ok := symbolKindNames[k]; ok {
		return name
	}
	return "unknown"
}
