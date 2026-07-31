package lsp

import (
	"context"
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
			if kind == lexicalSpanLiteral && structure.transition(declarationValue) == declarationTransitionRejected {
				return 0, false, nil
			}
			i = next
			continue
		}
		if source[i] == '/' {
			return 0, false, nil
		}
		if token, tokenEnd, ok := declarationRecognizedTokenAt(source, i, end); ok {
			switch structure.transition(token) {
			case declarationTransitionRejected:
				return 0, false, nil
			case declarationTransitionBody:
				return i, true, nil
			}
			i = tokenEnd
			continue
		}

		r, size := utf8.DecodeRuneInString(source[i:end])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		if isIdentifierByte(source[i]) {
			next = i + 1
			for next < end && isIdentifierByte(source[next]) {
				next++
			}
			if structure.transition(declarationValue) == declarationTransitionRejected {
				return 0, false, nil
			}
			i = next
			continue
		}
		if structure.transition(declarationContinuation) == declarationTransitionRejected {
			return 0, false, nil
		}
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
