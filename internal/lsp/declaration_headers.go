package lsp

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/skflowne/portolan/internal/core"
)

type lexicalSpanKind uint8

const (
	lexicalSpanNone lexicalSpanKind = iota
	lexicalSpanComment
	lexicalSpanLiteral
)

func extractDeclarationHeader(ctx context.Context, source string, symbol core.Symbol) (string, bool, error) {
	var keyword string
	switch symbol.Kind {
	case core.SymbolKindClass:
		keyword = "class"
	case core.SymbolKindInterface:
		keyword = "interface"
	default:
		return "", false, nil
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}

	start, err := byteOffsetContext(ctx, source, symbol.Range.Start)
	if err != nil {
		return "", false, err
	}
	end, err := byteOffsetContext(ctx, source, symbol.Range.End)
	if err != nil {
		return "", false, err
	}
	if end < start {
		return "", false, nil
	}

	keywordOffset, ok, err := declarationTokenOffset(ctx, source, start, end, []string{keyword})
	if err != nil || !ok {
		return "", false, err
	}
	nameOffset, ok, err := skipDeclarationTrivia(ctx, source, keywordOffset+len(keyword), end)
	if err != nil || !ok || symbol.Name == "" || !strings.HasPrefix(source[nameOffset:end], symbol.Name) ||
		!tokenBoundary(source, nameOffset, symbol.Name) {
		return "", false, err
	}

	bodyOffset, ok, err := declarationBodyOffset(ctx, source, nameOffset+len(symbol.Name), end)
	if err != nil || !ok {
		return "", false, err
	}
	header, ok, err := normalizeDeclarationHeader(ctx, source, keywordOffset, bodyOffset)
	if err != nil || !ok {
		return "", false, err
	}
	return header, true, nil
}

func declarationTokenOffset(ctx context.Context, source string, start, end int, tokens []string) (int, bool, error) {
	parenDepth, bracketDepth, braceDepth := 0, 0, 0
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
			i = next
			continue
		}

		switch source[i] {
		case '(':
			parenDepth++
		case ')':
			if parenDepth > 0 {
				parenDepth--
			}
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth > 0 {
				bracketDepth--
			}
		case '{':
			braceDepth++
		case '}':
			if braceDepth > 0 {
				braceDepth--
			}
		}
		if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 {
			for _, token := range tokens {
				if strings.HasPrefix(source[i:end], token) && tokenBoundary(source, i, token) {
					return i, true, nil
				}
			}
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

func skipDeclarationTrivia(ctx context.Context, source string, start, end int) (int, bool, error) {
	for i := start; i < end; {
		if err := scanContext(ctx, i-start); err != nil {
			return 0, false, err
		}
		next, kind, complete, err := scanLexicalSpan(ctx, source, i, end)
		if err != nil {
			return 0, false, err
		}
		if kind == lexicalSpanComment {
			if !complete {
				return 0, false, nil
			}
			i = next
			continue
		}
		if kind == lexicalSpanLiteral {
			return 0, false, nil
		}
		r, size := utf8.DecodeRuneInString(source[i:end])
		if !unicode.IsSpace(r) {
			return i, true, nil
		}
		i += size
	}
	return 0, false, nil
}

func declarationBodyOffset(ctx context.Context, source string, start, end int) (int, bool, error) {
	parenDepth, bracketDepth, braceDepth, angleDepth := 0, 0, 0, 0
	last := "value"
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
			if kind == lexicalSpanLiteral {
				last = "value"
			}
			i = next
			continue
		}
		if keyword, ok := declarationKeywordAt(source, i, end); ok {
			if last == "." {
				last = "value"
			} else {
				last = keyword
			}
			i += len(keyword)
			continue
		}

		switch source[i] {
		case '(':
			parenDepth++
			last = "("
		case ')':
			if parenDepth == 0 {
				return 0, false, nil
			}
			parenDepth--
			last = "value"
		case '[':
			bracketDepth++
			last = "["
		case ']':
			if bracketDepth == 0 {
				return 0, false, nil
			}
			bracketDepth--
			last = "value"
		case '<':
			if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 {
				angleDepth++
				last = "<"
			}
		case '>':
			if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 {
				if i > start && source[i-1] == '=' {
					last = "=>"
					break
				}
				if angleDepth == 0 || last != "," && incompleteDeclarationToken(last) {
					return 0, false, nil
				}
				angleDepth--
				last = "value"
			}
		case '{':
			if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 && angleDepth == 0 {
				if incompleteDeclarationToken(last) {
					return 0, false, nil
				}
				return i, true, nil
			}
			braceDepth++
			last = "{"
		case '}':
			if braceDepth == 0 {
				return 0, false, nil
			}
			braceDepth--
			last = "value"
		case ';':
			if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 && angleDepth == 0 {
				return 0, false, nil
			}
			last = "value"
		case ',', '?', ':', '=', '&', '|', '.':
			if source[i] == ',' && last == "," {
				return 0, false, nil
			}
			last = string(source[i])
		default:
			r, _ := utf8.DecodeRuneInString(source[i:end])
			if !unicode.IsSpace(r) {
				last = "value"
			}
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

func incompleteDeclarationToken(token string) bool {
	switch token {
	case "extends", "implements", "keyof", "typeof", "infer", "readonly", "new", "abstract",
		"(", "[", "{", "<", ",", "?", ":", "=", "=>", "&", "|", ".":
		return true
	default:
		return false
	}
}

func normalizeDeclarationHeader(ctx context.Context, source string, start, end int) (string, bool, error) {
	var normalized strings.Builder
	pendingSpace := false
	for i := start; i < end; {
		if err := scanContext(ctx, i-start); err != nil {
			return "", false, err
		}
		next, kind, complete, err := scanLexicalSpan(ctx, source, i, end)
		if err != nil {
			return "", false, err
		}
		switch kind {
		case lexicalSpanComment:
			if !complete {
				return "", false, nil
			}
			pendingSpace = true
			i = next
			continue
		case lexicalSpanLiteral:
			if !complete {
				return "", false, nil
			}
			appendDeclarationSpace(&normalized, &pendingSpace)
			normalized.WriteString(source[i:next])
			i = next
			continue
		}

		r, size := utf8.DecodeRuneInString(source[i:end])
		if unicode.IsSpace(r) {
			pendingSpace = true
			i += size
			continue
		}
		appendDeclarationSpace(&normalized, &pendingSpace)
		normalized.WriteString(source[i : i+size])
		i += size
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	header := normalized.String()
	return header, header != "", nil
}

func appendDeclarationSpace(normalized *strings.Builder, pending *bool) {
	if *pending && normalized.Len() > 0 {
		normalized.WriteByte(' ')
	}
	*pending = false
}

func scanLexicalSpan(ctx context.Context, source string, start, end int) (int, lexicalSpanKind, bool, error) {
	if source[start] == '/' && start+1 < end {
		switch source[start+1] {
		case '/':
			for i := start + 2; i < end; i++ {
				if err := scanContext(ctx, i-start); err != nil {
					return 0, lexicalSpanNone, false, err
				}
				if source[i] == '\n' {
					return i + 1, lexicalSpanComment, true, nil
				}
			}
			return end, lexicalSpanComment, true, nil
		case '*':
			for i := start + 2; i+1 < end; i++ {
				if err := scanContext(ctx, i-start); err != nil {
					return 0, lexicalSpanNone, false, err
				}
				if source[i] == '*' && source[i+1] == '/' {
					return i + 2, lexicalSpanComment, true, nil
				}
			}
			return end, lexicalSpanComment, false, nil
		}
	}

	quote := source[start]
	if quote != '\'' && quote != '"' && quote != '`' {
		return start, lexicalSpanNone, true, nil
	}
	for i := start + 1; i < end; i++ {
		if err := scanContext(ctx, i-start); err != nil {
			return 0, lexicalSpanNone, false, err
		}
		if source[i] == '\\' {
			i++
			continue
		}
		if source[i] == quote {
			return i + 1, lexicalSpanLiteral, true, nil
		}
	}
	return end, lexicalSpanLiteral, false, nil
}

func tokenBoundary(source string, start int, token string) bool {
	if token == "=>" {
		return true
	}
	beforeOK := start == 0 || !isIdentifierByte(source[start-1])
	end := start + len(token)
	afterOK := end == len(source) || !isIdentifierByte(source[end])
	return beforeOK && afterOK
}

func isIdentifierByte(b byte) bool {
	return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= utf8.RuneSelf
}

func scanContext(ctx context.Context, scanned int) error {
	if scanned > 0 && scanned%256 == 0 {
		return ctx.Err()
	}
	return nil
}
