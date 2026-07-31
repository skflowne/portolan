package lsp

import "github.com/skflowne/portolan/internal/core"

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
	declarationConditionalCondition declarationConditionalStage = iota + 1
	declarationConditionalTrue
	declarationConditionalTrueComplete
	declarationConditionalFalse
	declarationConditionalFalseComplete
)

type declarationGenericPurpose uint8

const (
	declarationGenericTypeList declarationGenericPurpose = iota + 1
	declarationGenericParameters
	declarationGenericCandidate
)

type declarationParameterStage uint8

const (
	declarationParameterName declarationParameterStage = iota
	declarationParameterConstraint
	declarationParameterDefault
)

type declarationHeritagePhase uint8

const (
	declarationBeforeHeritage declarationHeritagePhase = iota
	declarationExtendsHeritage
	declarationImplementsHeritage
)

type declarationContext struct {
	kind             declarationContextKind
	last             string
	typeContext      bool
	conditionals     []declarationConditionalStage
	genericPurpose   declarationGenericPurpose
	parameterStage   declarationParameterStage
	candidateInvalid bool
	requiresArrow    bool
}

type declarationStructure struct {
	contexts    []declarationContext
	symbolKind  core.SymbolKind
	heritage    declarationHeritagePhase
	genericSeen bool
}

func newDeclarationStructure(kind core.SymbolKind) declarationStructure {
	return declarationStructure{
		contexts:   []declarationContext{{kind: declarationRoot, last: "value"}},
		symbolKind: kind,
	}
}

func (s *declarationStructure) current() *declarationContext {
	return &s.contexts[len(s.contexts)-1]
}

func (s *declarationStructure) atRoot() bool {
	return len(s.contexts) == 1
}

func (s *declarationStructure) open(kind declarationContextKind, token string) bool {
	if s.current().last == "pendingArrow" || s.atRoot() && !s.inHeritageOperand() {
		return false
	}
	s.contexts = append(s.contexts, declarationContext{
		kind: kind, last: token, typeContext: s.current().typeContext,
	})
	return true
}

func (s *declarationStructure) close(kind declarationContextKind) bool {
	for len(s.contexts) > 1 && s.current().kind == declarationGeneric && s.current().genericPurpose == declarationGenericCandidate {
		if s.current().candidateInvalid || !s.current().canClose() {
			return false
		}
		s.contexts = s.contexts[:len(s.contexts)-1]
		if !s.markValue() {
			return false
		}
	}
	if len(s.contexts) == 1 || s.current().kind != kind || !s.current().canClose() {
		return false
	}
	requiresArrow := s.current().requiresArrow ||
		s.current().kind == declarationParen && s.current().typeContext && s.current().last == "("
	s.contexts = s.contexts[:len(s.contexts)-1]
	if requiresArrow {
		s.current().last = "pendingArrow"
		return true
	}
	return s.markValue()
}

func (s *declarationStructure) markValue() bool {
	current := s.current()
	if current.last == "optional" || current.last == "pendingArrow" || s.atRoot() && !s.inHeritageOperand() {
		return false
	}
	current.completeConditionalValue()
	current.last = "value"
	return true
}

func (c *declarationContext) completeConditionalValue() {
	if len(c.conditionals) == 0 {
		return
	}
	last := len(c.conditionals) - 1
	switch c.conditionals[last] {
	case declarationConditionalTrue:
		c.conditionals[last] = declarationConditionalTrueComplete
	case declarationConditionalFalse:
		c.conditionals[last] = declarationConditionalFalseComplete
	}
}

func (c *declarationContext) reduceCompletedConditionals() {
	for len(c.conditionals) > 0 && c.conditionals[len(c.conditionals)-1] == declarationConditionalFalseComplete {
		c.conditionals = c.conditionals[:len(c.conditionals)-1]
		c.completeConditionalValue()
	}
}

func (s *declarationStructure) markKeyword(keyword string) bool {
	current := s.current()
	if current.last == "pendingArrow" {
		return false
	}
	if current.last == "." {
		return s.markValue()
	}
	if s.atRoot() {
		return s.acceptHeritageKeyword(keyword)
	}
	if keyword == "extends" && current.typeContext && current.last == "value" {
		if current.genericPurpose == declarationGenericParameters && current.parameterStage == declarationParameterName {
			current.parameterStage = declarationParameterConstraint
			current.last = keyword
			return true
		}
		current.conditionals = append(current.conditionals, declarationConditionalCondition)
		current.last = keyword
		return true
	}
	current.last = keyword
	return true
}

func (s *declarationStructure) acceptHeritageKeyword(keyword string) bool {
	root := s.current()
	switch keyword {
	case "extends":
		if s.heritage != declarationBeforeHeritage {
			return false
		}
		s.heritage = declarationExtendsHeritage
	case "implements":
		if s.symbolKind != core.SymbolKindClass || s.heritage == declarationImplementsHeritage ||
			s.heritage == declarationExtendsHeritage && incompleteDeclarationToken(root.last) {
			return false
		}
		s.heritage = declarationImplementsHeritage
	default:
		return false
	}
	root.last = keyword
	return true
}

func (s *declarationStructure) markQuestion() bool {
	current := s.current()
	if current.typeContext && current.last == "value" && len(current.conditionals) > 0 {
		last := len(current.conditionals) - 1
		if current.conditionals[last] == declarationConditionalCondition {
			current.conditionals[last] = declarationConditionalTrue
			current.last = "?"
			return true
		}
	}
	if current.typeContext && current.last == "value" &&
		(current.kind == declarationBracket || current.kind == declarationBrace) {
		s.markGenericCandidateInvalid()
		return true
	}
	if current.typeContext && current.last == "value" && current.kind == declarationParen {
		current.requiresArrow = true
		current.last = "optional"
		return true
	}
	if current.typeContext && current.genericPurpose != declarationGenericCandidate || incompleteDeclarationToken(current.last) {
		return false
	}
	current.last = "?"
	return true
}

func (s *declarationStructure) markGenericCandidateInvalid() {
	for i := len(s.contexts) - 1; i >= 0; i-- {
		if s.contexts[i].kind == declarationGeneric {
			s.contexts[i].candidateInvalid = true
			return
		}
	}
}

func (s *declarationStructure) markColon() bool {
	current := s.current()
	if current.last == "optional" {
		current.last = ":"
		return true
	}
	current.reduceCompletedConditionals()
	conditional := len(current.conditionals) > 0
	if conditional {
		last := len(current.conditionals) - 1
		if current.conditionals[last] != declarationConditionalTrueComplete {
			return false
		}
		current.conditionals[last] = declarationConditionalFalse
	} else if incompleteDeclarationToken(current.last) {
		return false
	}
	if !conditional && current.kind == declarationParen && current.typeContext {
		current.requiresArrow = true
	}
	current.last = ":"
	return true
}

func (s *declarationStructure) markOperator(token string) bool {
	current := s.current()
	if current.last == "pendingArrow" {
		if token != "=>" {
			return false
		}
		current.last = token
		return true
	}
	if (token == "&" || token == "|") && current.last == token {
		return false
	}
	if token == "=" && current.genericPurpose == declarationGenericParameters {
		if current.parameterStage == declarationParameterDefault || incompleteDeclarationToken(current.last) {
			return false
		}
		current.parameterStage = declarationParameterDefault
	}
	current.last = token
	return true
}

func (s *declarationStructure) openGeneric() bool {
	current := s.current()
	purpose := declarationGenericTypeList
	switch {
	case s.atRoot() && s.heritage == declarationBeforeHeritage && !s.genericSeen:
		purpose = declarationGenericParameters
		s.genericSeen = true
	case s.atRoot() && s.inHeritageOperand() && current.last == "value":
	case current.kind == declarationGeneric:
	case current.last == "value" && current.typeContext:
	case current.last == "value":
		purpose = declarationGenericCandidate
	default:
		return false
	}
	s.contexts = append(s.contexts, declarationContext{
		kind: declarationGeneric, last: "<", typeContext: true, genericPurpose: purpose,
	})
	return true
}

func (s *declarationStructure) acceptSeparator() bool {
	current := s.current()
	if current.last == "pendingArrow" {
		return false
	}
	current.reduceCompletedConditionals()
	if len(current.conditionals) != 0 {
		return false
	}
	if s.atRoot() {
		if !s.heritageAllowsList() || incompleteDeclarationToken(current.last) {
			return false
		}
		current.last = ","
		return true
	}
	if current.requiresListOperand() && !current.hasListOperand() {
		return false
	}
	current.last = ","
	if current.genericPurpose == declarationGenericParameters {
		current.parameterStage = declarationParameterName
	}
	return true
}

func (s *declarationStructure) heritageAllowsList() bool {
	return s.symbolKind == core.SymbolKindInterface && s.heritage == declarationExtendsHeritage ||
		s.symbolKind == core.SymbolKindClass && s.heritage == declarationImplementsHeritage
}

func (s *declarationStructure) inHeritageOperand() bool {
	return s.heritage == declarationExtendsHeritage || s.heritage == declarationImplementsHeritage
}

func (c *declarationContext) requiresListOperand() bool {
	return c.kind != declarationBracket
}

func (c *declarationContext) hasListOperand() bool {
	return c.last != "," && !incompleteDeclarationToken(c.last)
}

func (c *declarationContext) canClose() bool {
	c.reduceCompletedConditionals()
	if len(c.conditionals) != 0 {
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
	purpose := current.genericPurpose
	s.contexts = s.contexts[:len(s.contexts)-1]
	if s.atRoot() && purpose == declarationGenericParameters {
		s.current().last = "value"
		return true
	}
	return s.markValue()
}

func (s *declarationStructure) canOpenBody() bool {
	return s.atRoot() && !incompleteDeclarationToken(s.current().last)
}

func incompleteDeclarationToken(token string) bool {
	switch token {
	case "extends", "implements", "keyof", "typeof", "infer", "readonly", "new", "abstract",
		"(", "[", "{", "<", ",", "?", "optional", "pendingArrow", ":", "=", "=>", "&", "|", ".":
		return true
	default:
		return false
	}
}
