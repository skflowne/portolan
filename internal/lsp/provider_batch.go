package lsp

import (
	"context"
	"sync"
)

const maxConcurrentProviderRequests = 8

type providerBatchWork func(context.Context, int) error

func runProviderBatch(ctx context.Context, indexes []int, work providerBatchWork) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(indexes) == 0 {
		return nil
	}

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	recordError := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	workerCount := min(len(indexes), maxConcurrentProviderRequests)
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if err := workCtx.Err(); err != nil {
					return
				}
				err := work(workCtx, index)
				if err == nil {
					err = workCtx.Err()
				}
				if err != nil {
					recordError(err)
					return
				}
			}
		}()
	}

dispatch:
	for _, index := range indexes {
		select {
		case jobs <- index:
		case <-workCtx.Done():
			break dispatch
		}
		if workCtx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
