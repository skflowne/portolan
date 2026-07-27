package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
)

// readFrame reads one LSP message off r: a block of headers terminated by a
// blank line, followed by exactly Content-Length bytes of JSON body.
func readFrame(r *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(parts[0]), "Content-Length") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("lsp: bad Content-Length %q: %w", strings.TrimSpace(parts[1]), err)
		}
		contentLength = n
	}
	if contentLength < 0 {
		return nil, errors.New("lsp: message missing Content-Length header")
	}
	buf := make([]byte, contentLength)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

type contextWorkResult[T any] struct {
	value T
	err   error
}

// runContextWork bounds pure in-memory work that has no context-aware API.
// The buffered result lets canceled callers return while finite work exits.
func runContextWork[T any](ctx context.Context, work func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	result := make(chan contextWorkResult[T], 1)
	go func() {
		value, err := work()
		result <- contextWorkResult[T]{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case got := <-result:
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return got.value, got.err
	}
}

func marshalMessage(ctx context.Context, v any) ([]byte, error) {
	return runContextWork(ctx, func() ([]byte, error) {
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("lsp: marshaling message: %w", err)
		}
		return data, nil
	})
}

type frameWriteResult struct {
	written int
	err     error
}

func frameBytes(data []byte) []byte {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	frame := make([]byte, 0, len(header)+len(data))
	frame = append(frame, header...)
	return append(frame, data...)
}

func writeFull(w io.Writer, data []byte, complete func()) (int, error) {
	written := 0
	for written < len(data) {
		n, err := w.Write(data[written:])
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	complete()
	return written, nil
}

const (
	frameWriting uint32 = iota
	frameDispatched
	frameAborted
)

func (t *transport) writeFrameLocked(ctx context.Context, data []byte) (bool, error) {
	frame, err := runContextWork(ctx, func() ([]byte, error) {
		return frameBytes(data), nil
	})
	if err != nil {
		return false, err
	}
	result := make(chan frameWriteResult, 1)
	var state atomic.Uint32
	go func() {
		written, err := writeFull(t.input, frame, func() {
			if state.CompareAndSwap(frameWriting, frameDispatched) && t.afterFrameDispatch != nil {
				t.afterFrameDispatch()
			}
		})
		result <- frameWriteResult{written: written, err: err}
	}()

	select {
	case got := <-result:
		if got.err != nil {
			state.CompareAndSwap(frameWriting, frameAborted)
			t.abort(fmt.Errorf("writing LSP frame after %d bytes: %w", got.written, got.err))
			return false, got.err
		}
		if state.Load() != frameDispatched {
			return false, ctx.Err()
		}
		return true, nil
	case <-ctx.Done():
		if !state.CompareAndSwap(frameWriting, frameAborted) {
			got := <-result
			if got.err != nil {
				t.abort(fmt.Errorf("writing LSP frame after %d bytes: %w", got.written, got.err))
				return false, got.err
			}
			return true, nil
		}
		t.abort(ctx.Err())
		return false, ctx.Err()
	}
}
