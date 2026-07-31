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
	last             declarationToken
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
		contexts:   []declarationContext{{kind: declarationRoot, last: declarationValue}},
		symbolKind: kind,
	}
}

func (s *declarationStructure) current() *declarationContext {
	return &s.contexts[len(s.contexts)-1]
}

func (s *declarationStructure) atRoot() bool {
	return len(s.contexts) == 1
}

func (s *declarationStructure) open(kind declarationContextKind, token declarationToken) bool {
	if s.current().last == declarationPendingArrow || s.atRoot() && !s.inHeritageOperand() {
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
		if !s.completeValue() {
			return false
		}
	}
	if len(s.contexts) == 1 || s.current().kind != kind || !s.current().canClose() {
		return false
	}
	requiresArrow := s.current().requiresArrow ||
		s.current().kind == declarationParen && s.current().typeContext && s.current().last == declarationOpenParen
	s.contexts = s.contexts[:len(s.contexts)-1]
	if requiresArrow {
		s.current().last = declarationPendingArrow
		return true
	}
	return s.completeValue()
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

func (s *declarationStructure) markKeyword(token declarationToken) bool {
	current := s.current()
	if current.last == declarationPendingArrow {
		return false
	}
	if current.last == declarationDot {
		return s.completeValue()
	}
	if s.atRoot() {
		return s.acceptHeritageKeyword(token)
	}
	if token == declarationExtends && current.typeContext && current.last == declarationValue {
		if current.genericPurpose == declarationGenericParameters && current.parameterStage == declarationParameterName {
			current.parameterStage = declarationParameterConstraint
			current.last = token
			return true
		}
		current.conditionals = append(current.conditionals, declarationConditionalCondition)
		current.last = token
		return true
	}
	current.last = token
	return true
}

func (s *declarationStructure) acceptHeritageKeyword(token declarationToken) bool {
	root := s.current()
	switch token {
	case declarationExtends:
		if s.heritage != declarationBeforeHeritage {
			return false
		}
		s.heritage = declarationExtendsHeritage
	case declarationImplements:
		if s.symbolKind != core.SymbolKindClass || s.heritage == declarationImplementsHeritage ||
			s.heritage == declarationExtendsHeritage && !root.last.metadata().terminalComplete {
			return false
		}
		s.heritage = declarationImplementsHeritage
	default:
		return false
	}
	root.last = token
	return true
}

func (s *declarationStructure) markQuestion() bool {
	current := s.current()
	if current.typeContext && current.last == declarationValue && len(current.conditionals) > 0 {
		last := len(current.conditionals) - 1
		if current.conditionals[last] == declarationConditionalCondition {
			current.conditionals[last] = declarationConditionalTrue
			current.last = declarationQuestion
			return true
		}
	}
	if current.typeContext && current.last == declarationValue &&
		(current.kind == declarationBracket || current.kind == declarationBrace) {
		s.markGenericCandidateInvalid()
		return true
	}
	if current.typeContext && current.last == declarationValue && current.kind == declarationParen {
		current.requiresArrow = true
		current.last = declarationOptional
		return true
	}
	if current.typeContext && current.genericPurpose != declarationGenericCandidate || current.last.metadata().requiresOperand {
		return false
	}
	current.last = declarationQuestion
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
	if current.last == declarationOptional {
		current.last = declarationColon
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
	} else if current.last.metadata().requiresOperand {
		return false
	}
	if !conditional && current.kind == declarationParen && current.typeContext {
		current.requiresArrow = true
	}
	current.last = declarationColon
	return true
}

func (s *declarationStructure) markOperator(token declarationToken) bool {
	current := s.current()
	if current.last == declarationPendingArrow {
		if token != declarationArrow {
			return false
		}
		current.last = token
		return true
	}
	if (current.last == declarationIntersection || current.last == declarationUnion) &&
		(token == declarationIntersection || token == declarationUnion) {
		return false
	}
	if token == declarationEqual && current.genericPurpose == declarationGenericParameters {
		if current.parameterStage == declarationParameterDefault || current.last.metadata().requiresOperand {
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
	case s.atRoot() && s.inHeritageOperand() && current.last == declarationValue:
	case current.kind == declarationGeneric:
	case current.last == declarationValue && current.typeContext:
	case current.last == declarationValue:
		purpose = declarationGenericCandidate
	default:
		return false
	}
	s.contexts = append(s.contexts, declarationContext{
		kind: declarationGeneric, last: declarationOpenGeneric, typeContext: true, genericPurpose: purpose,
	})
	return true
}

func (s *declarationStructure) acceptSeparator() bool {
	current := s.current()
	if current.last == declarationPendingArrow {
		return false
	}
	current.reduceCompletedConditionals()
	if len(current.conditionals) != 0 {
		return false
	}
	if s.atRoot() {
		if !s.heritageAllowsList() || !current.last.metadata().terminalComplete {
			return false
		}
		current.last = declarationComma
		return true
	}
	if current.requiresListOperand() && !current.hasListOperand() {
		return false
	}
	current.last = declarationComma
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
	return c.last != declarationComma && !c.last.metadata().requiresOperand
}

func (c *declarationContext) canClose() bool {
	c.reduceCompletedConditionals()
	if len(c.conditionals) != 0 {
		return false
	}
	if c.last == declarationComma || c.last == declarationSemicolon {
		return true
	}
	if c.last == contextOpeningToken(c.kind) {
		return c.kind != declarationGeneric
	}
	return c.last.metadata().terminalComplete
}

func contextOpeningToken(kind declarationContextKind) declarationToken {
	switch kind {
	case declarationGeneric:
		return declarationOpenGeneric
	case declarationParen:
		return declarationOpenParen
	case declarationBracket:
		return declarationOpenBracket
	case declarationBrace:
		return declarationOpenBrace
	default:
		return declarationValue
	}
}

func (s *declarationStructure) markGreaterThan() bool {
	if s.current().kind == declarationGeneric {
		return s.closeGeneric()
	}
	return !s.atRoot()
}

func (s *declarationStructure) closeGeneric() bool {
	current := s.current()
	if current.kind != declarationGeneric || !current.canClose() {
		return false
	}
	purpose := current.genericPurpose
	s.contexts = s.contexts[:len(s.contexts)-1]
	if s.atRoot() && purpose == declarationGenericParameters {
		s.current().last = declarationValue
		return true
	}
	return s.completeValue()
}

func (s *declarationStructure) canOpenBody() bool {
	return s.atRoot() && s.current().last.metadata().terminalComplete
}
