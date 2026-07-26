package testinfra

import (
	"bytes"
	"io"
	"os"
	"sync"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func captureProcessStdout(source *os.File, forward *io.PipeWriter, capture *lockedBuffer, done chan<- struct{}) {
	defer close(done)
	defer source.Close()
	defer forward.Close()
	buffer := make([]byte, 32*1024)
	forwarding := true
	for {
		n, err := source.Read(buffer)
		if n > 0 {
			_, _ = capture.Write(buffer[:n])
			if forwarding {
				if _, writeErr := forward.Write(buffer[:n]); writeErr != nil {
					forwarding = false
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// StdoutBytes returns the exact MCP stdout bytes consumed through the single reader.
func (d *Daemon) StdoutBytes() []byte { return d.stdout.Bytes() }

// StdoutString returns the captured MCP stdout as text.
func (d *Daemon) StdoutString() string { return d.stdout.String() }

// FinishStdout closes the MCP-facing reader after process exit and waits until
// the sole source reader has captured the process stream through EOF.
func (d *Daemon) FinishStdout(timeout time.Duration) bool {
	_ = d.Stdout.Close()
	select {
	case <-d.stdoutDone:
		return true
	case <-time.After(timeout):
		return false
	}
}
