package lsp

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

const (
	defaultShutdownTimeout = 2 * time.Second
	defaultExitWait        = 3 * time.Second
	defaultKillWait        = time.Second
)

// Close releases pending callers, performs the best-effort LSP shutdown
// handshake, and stops the subprocess. Concurrent calls share one cleanup.
func (p *Provider) Close() error {
	return p.transport.Close()
}

func (t *transport) Close() error {
	t.closeOnce.Do(func() {
		t.closeErr = t.close()
	})
	return t.closeErr
}

func (t *transport) close() error {
	shutdownTimeout := t.shutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	exitWait := t.exitWait
	if exitWait <= 0 {
		exitWait = defaultExitWait
	}
	killWait := t.killWait
	if killWait <= 0 {
		killWait = defaultKillWait
	}
	totalCtx, totalCancel := context.WithTimeout(context.Background(), shutdownTimeout+exitWait+killWait)
	defer totalCancel()
	ctx, cancel := context.WithTimeout(totalCtx, shutdownTimeout)
	defer cancel()

	id := t.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	data, marshalErr := marshalMessage(ctx, rpcRequest{JSONRPC: "2.0", ID: id, Method: "shutdown", Params: nil})

	var shutdown *pendingRequest
	var started bool
	var writeErr error
	if err := t.lockWrite(ctx); err != nil {
		t.abort(err)
	} else {
		shutdown, started = t.beginClose(key)
		if started {
			if marshalErr != nil {
				writeErr = marshalErr
			} else {
				_, writeErr = t.writeFrameLocked(ctx, data)
			}
		}
		t.unlockWrite()
	}

	if started {
		if writeErr != nil {
			t.complete(key, pendingResult{err: fmt.Errorf("writing shutdown request: %w", writeErr)})
		}
		_, _ = t.waitPending(ctx, key, shutdown)
		_, _ = t.writeMessage(ctx, writeExit, rpcNotification{JSONRPC: "2.0", Method: "exit", Params: nil})
	}

	_ = t.closeInput()
	t.waitForProcessContext(totalCtx, exitWait, killWait)
	t.finishClose()
	return nil
}

func (t *transport) abortAndWait(cause error) {
	t.abort(cause)
	killWait := t.killWait
	if killWait <= 0 {
		killWait = defaultKillWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), killWait)
	defer cancel()
	t.waitForProcessContext(ctx, 0, killWait)
}

func (t *transport) waitForProcess(timeout time.Duration) {
	if timeout <= 0 {
		timeout = defaultExitWait
	}
	killWait := t.killWait
	if killWait <= 0 {
		killWait = defaultKillWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+killWait)
	defer cancel()
	t.waitForProcessContext(ctx, timeout, killWait)
}

func (t *transport) startProcessWait() <-chan struct{} {
	t.waitOnce.Do(func() {
		if t.waitProcess == nil {
			close(t.processDone)
			return
		}
		go func() {
			_ = t.waitProcess()
			close(t.processDone)
		}()
	})
	return t.processDone
}

func (t *transport) waitForProcessContext(ctx context.Context, gracefulWait, killWait time.Duration) {
	done := t.startProcessWait()
	if gracefulWait > 0 {
		timer := time.NewTimer(gracefulWait)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	t.kill()
	if killWait <= 0 {
		return
	}
	killTimer := time.NewTimer(killWait)
	defer killTimer.Stop()
	select {
	case <-done:
	case <-killTimer.C:
	case <-ctx.Done():
	}
}
