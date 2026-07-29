package lsp

import (
	"encoding/json"

	"github.com/skflowne/portolan/internal/core"
)

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
	Hover          *hoverClientCapabilities          `json:"hover,omitempty"`
}

type hoverClientCapabilities struct {
	ContentFormat []string `json:"contentFormat,omitempty"`
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

type hoverResult struct {
	Contents json.RawMessage `json:"contents"`
}

type markupContent struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Detail         string              `json:"detail,omitempty"`
	Kind           int                 `json:"kind"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children,omitempty"`
}

// symbolKindNames is the explicit adapter from LSP's numeric SymbolKind enum
// to core's provider-independent vocabulary.
var symbolKindNames = map[int]core.SymbolKind{
	1:  core.SymbolKindFile,
	2:  core.SymbolKindModule,
	3:  core.SymbolKindNamespace,
	4:  core.SymbolKindPackage,
	5:  core.SymbolKindClass,
	6:  core.SymbolKindMethod,
	7:  core.SymbolKindProperty,
	8:  core.SymbolKindField,
	9:  core.SymbolKindConstructor,
	10: core.SymbolKindEnum,
	11: core.SymbolKindInterface,
	12: core.SymbolKindFunction,
	13: core.SymbolKindVariable,
	14: core.SymbolKindConstant,
	15: core.SymbolKindString,
	16: core.SymbolKindNumber,
	17: core.SymbolKindBoolean,
	18: core.SymbolKindArray,
	19: core.SymbolKindObject,
	20: core.SymbolKindKey,
	21: core.SymbolKindNull,
	22: core.SymbolKindEnumMember,
	23: core.SymbolKindStruct,
	24: core.SymbolKindEvent,
	25: core.SymbolKindOperator,
	26: core.SymbolKindTypeParameter,
}

func symbolKindName(k int) core.SymbolKind {
	if name, ok := symbolKindNames[k]; ok {
		return name
	}
	return core.SymbolKindUnknown
}
