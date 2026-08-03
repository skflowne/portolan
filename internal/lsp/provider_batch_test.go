package lsp

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRunProviderBatchUsesInputIndexes(t *testing.T) {
	indexes := []int{4, 1, 3}
	results := make([]int, 5)
	calls := make([]int, 5)
	var mu sync.Mutex

	err := runProviderBatch(context.Background(), indexes, func(_ context.Context, index int) error {
		mu.Lock()
		defer mu.Unlock()
		results[index] = index * 10
		calls[index]++
		return nil
	})
	if err != nil {
		t.Fatalf("runProviderBatch: %v", err)
	}
	if want := []int{0, 10, 0, 30, 40}; !reflect.DeepEqual(results, want) {
		t.Fatalf("results = %v, want %v", results, want)
	}
	if want := []int{0, 1, 0, 1, 1}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestRunProviderBatchBoundsConcurrency(t *testing.T) {
	indexes := make([]int, maxConcurrentProviderRequests*2)
	for i := range indexes {
		indexes[i] = i
	}

	entered := make(chan struct{}, len(indexes))
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- runProviderBatch(context.Background(), indexes, func(context.Context, int) error {
			entered <- struct{}{}
			<-release
			return nil
		})
	}()

	for range maxConcurrentProviderRequests {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("provider batch did not reach its concurrency limit")
		}
	}
	select {
	case <-entered:
		close(release)
		t.Fatal("provider batch exceeded its concurrency limit")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("runProviderBatch: %v", err)
	}
	if got := len(entered) + maxConcurrentProviderRequests; got != len(indexes) {
		t.Fatalf("dispatched indexes = %d, want %d", got, len(indexes))
	}
}

func TestRunProviderBatchCancelsAndDrainsOnFirstError(t *testing.T) {
	firstErr := errors.New("first provider failure")
	siblingStarted := make(chan struct{})
	siblingDrained := make(chan struct{})

	err := runProviderBatch(context.Background(), []int{0, 1}, func(ctx context.Context, index int) error {
		switch index {
		case 0:
			<-siblingStarted
			return firstErr
		case 1:
			close(siblingStarted)
			<-ctx.Done()
			close(siblingDrained)
			return ctx.Err()
		default:
			return errors.New("unexpected index")
		}
	})
	if !errors.Is(err, firstErr) {
		t.Fatalf("runProviderBatch error = %v, want first provider failure", err)
	}
	select {
	case <-siblingDrained:
	default:
		t.Fatal("runProviderBatch returned before in-flight work drained")
	}
}

func TestRunProviderBatchRejectsCanceledContextBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := runProviderBatch(ctx, []int{0}, func(context.Context, int) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runProviderBatch error = %v, want context canceled", err)
	}
	if called {
		t.Fatal("provider batch dispatched work after cancellation")
	}
}
