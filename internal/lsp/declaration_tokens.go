package lsp

import "strings"

type declarationToken uint8

type declarationTokenCategory uint8

const (
	declarationTokenControl declarationTokenCategory = iota
	declarationTokenOperand
	declarationTokenContinuation
)

type declarationTokenBoundary uint8

const (
	declarationTokenNoBoundary declarationTokenBoundary = iota
	declarationTokenIdentifierBoundary
)

type declarationTokenMetadata struct {
	lexeme           string
	category         declarationTokenCategory
	boundary         declarationTokenBoundary
	requiresOperand  bool
	terminalComplete bool
}

const (
	declarationValue declarationToken = iota
	declarationContinuation
	declarationExtends
	declarationImplements
	declarationKeyof
	declarationTypeof
	declarationInfer
	declarationIn
	declarationReadonly
	declarationNew
	declarationAbstract
	declarationOpenParen
	declarationCloseParen
	declarationOpenBracket
	declarationCloseBracket
	declarationOpenBrace
	declarationCloseBrace
	declarationOpenGeneric
	declarationCloseGeneric
	declarationComma
	declarationQuestion
	declarationOptional
	declarationPendingArrow
	declarationColon
	declarationEqual
	declarationArrow
	declarationIntersection
	declarationUnion
	declarationDot
	declarationSemicolon
)

var declarationTokenMetadataByToken = [...]declarationTokenMetadata{
	declarationValue:        {category: declarationTokenOperand, terminalComplete: true},
	declarationContinuation: {category: declarationTokenContinuation, terminalComplete: true},
	declarationExtends:      {lexeme: "extends", boundary: declarationTokenIdentifierBoundary, requiresOperand: true},
	declarationImplements:   {lexeme: "implements", boundary: declarationTokenIdentifierBoundary, requiresOperand: true},
	declarationKeyof:        {lexeme: "keyof", boundary: declarationTokenIdentifierBoundary, requiresOperand: true},
	declarationTypeof:       {lexeme: "typeof", boundary: declarationTokenIdentifierBoundary, requiresOperand: true},
	declarationInfer:        {lexeme: "infer", boundary: declarationTokenIdentifierBoundary, requiresOperand: true},
	declarationIn:           {lexeme: "in", boundary: declarationTokenIdentifierBoundary, requiresOperand: true},
	declarationReadonly:     {lexeme: "readonly", boundary: declarationTokenIdentifierBoundary, requiresOperand: true},
	declarationNew:          {lexeme: "new", boundary: declarationTokenIdentifierBoundary, requiresOperand: true},
	declarationAbstract:     {lexeme: "abstract", boundary: declarationTokenIdentifierBoundary, requiresOperand: true},
	declarationOpenParen:    {lexeme: "(", requiresOperand: true},
	declarationCloseParen:   {lexeme: ")", terminalComplete: true},
	declarationOpenBracket:  {lexeme: "[", requiresOperand: true},
	declarationCloseBracket: {lexeme: "]", terminalComplete: true},
	declarationOpenBrace:    {lexeme: "{", requiresOperand: true},
	declarationCloseBrace:   {lexeme: "}", terminalComplete: true},
	declarationOpenGeneric:  {lexeme: "<", requiresOperand: true},
	declarationCloseGeneric: {lexeme: ">", terminalComplete: true},
	declarationComma:        {lexeme: ",", requiresOperand: true},
	declarationQuestion:     {lexeme: "?", requiresOperand: true},
	declarationOptional:     {requiresOperand: true},
	declarationPendingArrow: {requiresOperand: true},
	declarationColon:        {lexeme: ":", requiresOperand: true},
	declarationEqual:        {lexeme: "=", requiresOperand: true},
	declarationArrow:        {lexeme: "=>", requiresOperand: true},
	declarationIntersection: {lexeme: "&", requiresOperand: true},
	declarationUnion:        {lexeme: "|", requiresOperand: true},
	declarationDot:          {lexeme: ".", requiresOperand: true},
	declarationSemicolon:    {lexeme: ";", requiresOperand: true},
}

func declarationRecognizedTokenAt(source string, start, end int) (declarationToken, int, bool) {
	var matched declarationToken
	matchedLength := 0
	for token := declarationExtends; int(token) < len(declarationTokenMetadataByToken); token++ {
		metadata := declarationTokenMetadataByToken[token]
		if len(metadata.lexeme) <= matchedLength || !strings.HasPrefix(source[start:end], metadata.lexeme) {
			continue
		}
		if metadata.boundary == declarationTokenIdentifierBoundary && !tokenBoundary(source, start, metadata.lexeme) {
			continue
		}
		matched = token
		matchedLength = len(metadata.lexeme)
	}
	return matched, start + matchedLength, matchedLength != 0
}

func (t declarationToken) metadata() declarationTokenMetadata {
	return declarationTokenMetadataByToken[t]
}
