package lsp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestRunFiniteWorkCancellationPrecedence(t *testing.T) {
	t.Run("caller cancellation over completion", func(t *testing.T) {
		ctx := newCancelOnErrCheckContext(2)
		value, err := runFiniteWork(ctx, nil, func() (string, error) {
			return "completed", nil
		})
		if !errors.Is(err, context.Canceled) || value != "" {
			t.Fatalf("runFiniteWork = (%q, %v), want no value and context canceled", value, err)
		}
	})

	t.Run("interruption over completion", func(t *testing.T) {
		interrupted := make(chan struct{})
		cause := errors.New("transport unavailable")
		value, err := runFiniteWork(context.Background(), &finiteWorkInterruption{
			done:  interrupted,
			cause: func() error { return cause },
		}, func() (string, error) {
			close(interrupted)
			return "completed", nil
		})
		if !errors.Is(err, cause) || value != "" {
			t.Fatalf("runFiniteWork = (%q, %v), want no value and interruption cause", value, err)
		}
	})

	t.Run("interruption prevents work", func(t *testing.T) {
		interrupted := make(chan struct{})
		close(interrupted)
		cause := errors.New("transport unavailable")
		var calls atomic.Int32
		_, err := runFiniteWork(context.Background(), &finiteWorkInterruption{
			done:  interrupted,
			cause: func() error { return cause },
		}, func() (string, error) {
			calls.Add(1)
			return "completed", nil
		})
		if !errors.Is(err, cause) {
			t.Fatalf("runFiniteWork error = %v, want interruption cause", err)
		}
		if got := calls.Load(); got != 0 {
			t.Fatalf("work calls = %d, want none", got)
		}
	})

	t.Run("caller cancellation over interruption", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		interrupted := make(chan struct{})
		close(interrupted)
		cause := errors.New("transport unavailable")
		var calls atomic.Int32
		_, err := runFiniteWork(ctx, &finiteWorkInterruption{
			done:  interrupted,
			cause: func() error { return cause },
		}, func() (string, error) {
			calls.Add(1)
			return "completed", nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runFiniteWork error = %v, want context canceled", err)
		}
		if got := calls.Load(); got != 0 {
			t.Fatalf("work calls = %d, want none", got)
		}
	})
}

func TestFiniteWorkResultPublicationIsBuffered(t *testing.T) {
	result := newFiniteWorkResult[string]()
	published := make(chan struct{})
	go func() {
		result <- finiteWorkResult[string]{value: "completed"}
		close(published)
	}()

	waitSignal(t, published, "unobserved finite work result publication")
	if got := <-result; got.value != "completed" || got.err != nil {
		t.Fatalf("published result = %+v, want completed value", got)
	}
}

func TestRunFiniteWorkReturnsBeforeLateCompletion(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := runFiniteWork(ctx, nil, func() (struct{}, error) {
			close(entered)
			<-release
			close(exited)
			return struct{}{}, nil
		})
		result <- err
	}()

	waitSignal(t, entered, "finite work start")
	cancel()
	if err := waitError(t, result, "finite work cancellation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("runFiniteWork error = %v, want context canceled", err)
	}
	close(release)
	waitSignal(t, exited, "late finite work completion")
}
