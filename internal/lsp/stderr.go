package lsp

import (
	"io"
	"strings"
	"sync"
)

// stderrBuffer captures recent subprocess diagnostics for transport errors.
type stderrBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const stderrBufferCap = 8192

func newStderrBuffer() *stderrBuffer { return &stderrBuffer{} }

func (b *stderrBuffer) drain(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.mu.Lock()
			b.buf = append(b.buf, buf[:n]...)
			if len(b.buf) > stderrBufferCap {
				b.buf = b.buf[len(b.buf)-stderrBufferCap:]
			}
			b.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (b *stderrBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
