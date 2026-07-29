package lsp

import "context"

type finiteWorkResult[T any] struct {
	value T
	err   error
}

type finiteWorkInterruption struct {
	done  <-chan struct{}
	cause func() error
}

func newFiniteWorkResult[T any]() chan finiteWorkResult[T] {
	return make(chan finiteWorkResult[T], 1)
}

// runFiniteWork owns completion-versus-cancellation arbitration for finite work
// whose underlying API cannot be interrupted. Caller cancellation takes
// precedence over interruption, and either takes precedence over completion.
func runFiniteWork[T any](
	ctx context.Context,
	interruption *finiteWorkInterruption,
	work func() (T, error),
) (T, error) {
	var zero T
	if err := finiteWorkCancellation(ctx, interruption); err != nil {
		return zero, err
	}

	result := newFiniteWorkResult[T]()
	go func() {
		value, err := work()
		result <- finiteWorkResult[T]{value: value, err: err}
	}()

	var interruptionDone <-chan struct{}
	if interruption != nil {
		interruptionDone = interruption.done
	}
	var completed finiteWorkResult[T]
	select {
	case completed = <-result:
	case <-ctx.Done():
	case <-interruptionDone:
	}
	if err := finiteWorkCancellation(ctx, interruption); err != nil {
		return zero, err
	}
	return completed.value, completed.err
}

func finiteWorkCancellation(ctx context.Context, interruption *finiteWorkInterruption) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if interruption == nil || interruption.done == nil {
		return nil
	}
	select {
	case <-interruption.done:
		return interruption.cause()
	default:
		return nil
	}
}
