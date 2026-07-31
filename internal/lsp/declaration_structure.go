package lsp

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

type declarationContextKind uint8

const (
	declarationRoot declarationContextKind = iota
	declarationGeneric
	declarationParen
	declarationBracket
	declarationBrace
)

type declarationConditionalStage uint8

const (
	declarationConditionalNone declarationConditionalStage = iota
	declarationConditionalCondition
	declarationConditionalTrue
	declarationConditionalTrueComplete
	declarationConditionalFalse
)

type declarationContext struct {
	kind        declarationContextKind
	last        string
	typeContext bool
	conditional declarationConditionalStage
	// Expression contexts treat an unmatched generic candidate as a relational operator.
	tentativeGeneric bool
}

type declarationStructure struct {
	contexts []declarationContext
}

func newDeclarationStructure() declarationStructure {
	return declarationStructure{contexts: []declarationContext{{kind: declarationRoot, last: "value"}}}
}

func (s *declarationStructure) current() *declarationContext {
	return &s.contexts[len(s.contexts)-1]
}

func (s *declarationStructure) open(kind declarationContextKind, token string) {
	s.contexts = append(s.contexts, declarationContext{
		kind: kind, last: token, typeContext: s.current().typeContext || kind == declarationGeneric,
	})
}

func (s *declarationStructure) close(kind declarationContextKind) bool {
	for len(s.contexts) > 1 && s.current().kind == declarationGeneric && s.current().tentativeGeneric {
		if !s.current().canClose() {
			return false
		}
		s.contexts = s.contexts[:len(s.contexts)-1]
		s.mark("value")
	}
	if len(s.contexts) == 1 || s.current().kind != kind || !s.current().canClose() {
		return false
	}
	s.contexts = s.contexts[:len(s.contexts)-1]
	s.mark("value")
	return true
}

func (s *declarationStructure) mark(token string) {
	current := s.current()
	if token == "value" {
		switch current.conditional {
		case declarationConditionalTrue:
			current.conditional = declarationConditionalTrueComplete
		case declarationConditionalFalse:
			current.conditional = declarationConditionalNone
		}
	}
	current.last = token
}

func (s *declarationStructure) markKeyword(keyword string) {
	current := s.current()
	if current.last == "." {
		s.mark("value")
		return
	}
	if keyword == "extends" && current.kind == declarationBracket && current.typeContext && current.last == "value" {
		current.conditional = declarationConditionalCondition
	}
	current.last = keyword
}

func (s *declarationStructure) markQuestion() {
	current := s.current()
	if current.kind == declarationBracket && current.typeContext && current.last == "value" {
		if current.conditional == declarationConditionalNone {
			return
		}
		if current.conditional == declarationConditionalCondition {
			current.conditional = declarationConditionalTrue
		}
	}
	current.last = "?"
}

func (s *declarationStructure) markColon() {
	current := s.current()
	if current.conditional == declarationConditionalTrueComplete {
		current.conditional = declarationConditionalFalse
	}
	current.last = ":"
}

func (s *declarationStructure) atRoot() bool {
	return s.current().kind == declarationRoot
}

func (s *declarationStructure) openGeneric() {
	currentKind := s.current().kind
	if currentKind == declarationRoot || currentKind == declarationGeneric {
		s.contexts = append(s.contexts, declarationContext{kind: declarationGeneric, last: "<", typeContext: true})
	} else if s.current().last == "value" {
		s.contexts = append(s.contexts, declarationContext{
			kind: declarationGeneric, last: "<", typeContext: true, tentativeGeneric: true,
		})
	}
}

func (s *declarationStructure) acceptSeparator() bool {
	current := s.current()
	if current.conditional != declarationConditionalNone || current.requiresListOperand() && !current.hasListOperand() {
		return false
	}
	current.last = ","
	return true
}

func (c *declarationContext) requiresListOperand() bool {
	switch c.kind {
	case declarationRoot, declarationGeneric, declarationParen, declarationBrace:
		return true
	case declarationBracket:
		return false
	default:
		return true
	}
}

func (c *declarationContext) hasListOperand() bool {
	return c.last != "," && !incompleteDeclarationToken(c.last)
}

func (c *declarationContext) canClose() bool {
	if c.conditional != declarationConditionalNone {
		return false
	}
	if c.last == "," {
		return true
	}
	if c.last == contextOpeningToken(c.kind) {
		return c.kind != declarationGeneric
	}
	return !incompleteDeclarationToken(c.last)
}

func contextOpeningToken(kind declarationContextKind) string {
	switch kind {
	case declarationGeneric:
		return "<"
	case declarationParen:
		return "("
	case declarationBracket:
		return "["
	case declarationBrace:
		return "{"
	default:
		return ""
	}
}

func (s *declarationStructure) closeGeneric() bool {
	current := s.current()
	if current.kind != declarationGeneric || !current.canClose() {
		return false
	}
	s.contexts = s.contexts[:len(s.contexts)-1]
	s.mark("value")
	return true
}

func declarationBodyOffset(ctx context.Context, source string, start, end int) (int, bool, error) {
	structure := newDeclarationStructure()
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
				structure.mark("value")
			}
			i = next
			continue
		}
		if keyword, ok := declarationKeywordAt(source, i, end); ok {
			structure.markKeyword(keyword)
			i += len(keyword)
			continue
		}

		switch source[i] {
		case '(':
			structure.open(declarationParen, "(")
		case ')':
			if !structure.close(declarationParen) {
				return 0, false, nil
			}
		case '[':
			structure.open(declarationBracket, "[")
		case ']':
			if !structure.close(declarationBracket) {
				return 0, false, nil
			}
		case '<':
			structure.openGeneric()
		case '>':
			if structure.current().kind == declarationGeneric {
				if i > start && source[i-1] == '=' {
					structure.mark("=>")
				} else if !structure.closeGeneric() {
					return 0, false, nil
				}
			} else if structure.atRoot() {
				if i <= start || source[i-1] != '=' {
					return 0, false, nil
				}
				structure.mark("=>")
			}
		case '{':
			if structure.atRoot() {
				if incompleteDeclarationToken(structure.current().last) {
					return 0, false, nil
				}
				return i, true, nil
			}
			structure.open(declarationBrace, "{")
		case '}':
			if !structure.close(declarationBrace) {
				return 0, false, nil
			}
		case ';':
			if structure.atRoot() {
				return 0, false, nil
			}
			structure.mark("value")
		case ',':
			if !structure.acceptSeparator() {
				return 0, false, nil
			}
		case '?':
			structure.markQuestion()
		case ':':
			structure.markColon()
		case '=', '&', '|', '.':
			structure.mark(string(source[i]))
		default:
			r, _ := utf8.DecodeRuneInString(source[i:end])
			if !unicode.IsSpace(r) {
				structure.mark("value")
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
