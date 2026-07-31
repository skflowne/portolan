package lsp

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/skflowne/portolan/internal/core"
)

func declarationBodyOffset(ctx context.Context, source string, start, end int, symbolKind core.SymbolKind) (int, bool, error) {
	structure := newDeclarationStructure(symbolKind)
	for i := start; i < end; {
		if err := scanContext(ctx, i-start); err != nil {
			return 0, false, err
		}
		next, kind, complete, err := scanLexicalSpan(ctx, source, i, end)
		if err != nil {
			return 0, false, err
		}
		if kind != lexicalSpanNone {
			if !complete {
				return 0, false, nil
			}
			if kind == lexicalSpanLiteral && !structure.markValue(false) {
				return 0, false, nil
			}
			i = next
			continue
		}
		if source[i] == '/' {
			return 0, false, nil
		}
		if keyword, ok := declarationKeywordAt(source, i, end); ok {
			if !structure.markKeyword(keyword) {
				return 0, false, nil
			}
			i += len(keyword)
			continue
		}

		accepted := true
		switch source[i] {
		case '(':
			accepted = structure.open(declarationParen, "(")
		case ')':
			accepted = structure.close(declarationParen)
		case '[':
			accepted = structure.open(declarationBracket, "[")
		case ']':
			accepted = structure.close(declarationBracket)
		case '<':
			accepted = structure.openGeneric()
		case '>':
			accepted = structure.markGreaterThan()
		case '{':
			if structure.atRoot() {
				if structure.canOpenBody() {
					return i, true, nil
				}
				return 0, false, nil
			}
			accepted = structure.open(declarationBrace, "{")
		case '}':
			accepted = structure.close(declarationBrace)
		case ';':
			if structure.atRoot() {
				return 0, false, nil
			}
			accepted = structure.markValue(false)
		case ',':
			accepted = structure.acceptSeparator()
		case '?':
			accepted = structure.markQuestion()
		case ':':
			accepted = structure.markColon()
		case '=':
			if i+1 < end && source[i+1] == '>' {
				accepted = structure.markOperator("=>")
				i++
			} else {
				accepted = structure.markOperator("=")
			}
		case '&', '|', '.':
			accepted = structure.markOperator(string(source[i]))
		default:
			r, _ := utf8.DecodeRuneInString(source[i:end])
			if !unicode.IsSpace(r) {
				tokenStart := isIdentifierByte(source[i]) && (i == start || !isIdentifierByte(source[i-1]))
				accepted = structure.markValue(tokenStart)
			}
		}
		if !accepted {
			return 0, false, nil
		}
		_, size := utf8.DecodeRuneInString(source[i:end])
		if size == 0 {
			break
		}
		i += size
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return 0, false, nil
}

func declarationKeywordAt(source string, start, end int) (string, bool) {
	for _, keyword := range []string{"extends", "implements", "keyof", "typeof", "infer", "readonly", "new", "abstract"} {
		if strings.HasPrefix(source[start:end], keyword) && tokenBoundary(source, start, keyword) {
			return keyword, true
		}
	}
	return "", false
}
