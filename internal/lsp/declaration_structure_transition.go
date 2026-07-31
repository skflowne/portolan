package lsp

type declarationTransition uint8

const (
	declarationTransitionRejected declarationTransition = iota
	declarationTransitionAccepted
	declarationTransitionBody
)

func (s *declarationStructure) markValue(token declarationToken) bool {
	current := s.current()
	if current.last == declarationOptional || current.last == declarationPendingArrow ||
		s.atRoot() && !s.inHeritageOperand() ||
		token.metadata().category == declarationTokenOperand && current.last == declarationValue {
		return false
	}
	current.completeConditionalValue()
	current.last = token
	return true
}

func (s *declarationStructure) completeValue() bool {
	current := s.current()
	if current.last == declarationOptional || current.last == declarationPendingArrow ||
		s.atRoot() && !s.inHeritageOperand() {
		return false
	}
	current.completeConditionalValue()
	current.last = declarationValue
	return true
}

func (s *declarationStructure) transition(token declarationToken) declarationTransition {
	accepted := false
	switch token {
	case declarationValue, declarationContinuation:
		accepted = s.markValue(token)
	case declarationExtends, declarationImplements, declarationKeyof, declarationTypeof,
		declarationInfer, declarationReadonly, declarationNew, declarationAbstract:
		accepted = s.markKeyword(token)
	case declarationOpenParen:
		accepted = s.open(declarationParen, token)
	case declarationCloseParen:
		accepted = s.close(declarationParen)
	case declarationOpenBracket:
		accepted = s.open(declarationBracket, token)
	case declarationCloseBracket:
		accepted = s.close(declarationBracket)
	case declarationOpenGeneric:
		accepted = s.openGeneric()
	case declarationCloseGeneric:
		accepted = s.markGreaterThan()
	case declarationOpenBrace:
		if s.atRoot() {
			if s.canOpenBody() {
				return declarationTransitionBody
			}
			return declarationTransitionRejected
		}
		accepted = s.open(declarationBrace, token)
	case declarationCloseBrace:
		accepted = s.close(declarationBrace)
	case declarationSemicolon:
		current := s.current()
		if !s.atRoot() && !current.last.metadata().requiresOperand {
			current.last = declarationSemicolon
			accepted = true
		}
	case declarationComma:
		accepted = s.acceptSeparator()
	case declarationQuestion:
		accepted = s.markQuestion()
	case declarationColon:
		accepted = s.markColon()
	case declarationEqual, declarationArrow, declarationIntersection, declarationUnion, declarationDot:
		accepted = s.markOperator(token)
	}
	if !accepted {
		return declarationTransitionRejected
	}
	return declarationTransitionAccepted
}
