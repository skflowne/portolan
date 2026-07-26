package mcp

import (
	"bufio"
	"fmt"
	"net"
	"strings"
)

// Wait blocks until the accept loop and all in-flight connections have
// finished. Call it after cancelling the context passed to Start.
func (c *ControlSocket) Wait() {
	c.acceptWG.Wait()
	c.handlers.Wait()
	c.cleanup()
}

func (c *ControlSocket) beginShutdown() {
	c.shutdownOnce.Do(func() {
		// Close the listener first. This unblocks Accept before the state lock
		// is changed, so an accepted connection can be classified atomically.
		if c.listener != nil {
			_ = c.listener.Close()
		}
		c.connMu.Lock()
		c.shuttingDown = true
		for conn := range c.connections {
			_ = conn.Close()
		}
		c.connMu.Unlock()
	})
}

func (c *ControlSocket) acceptLoop() {
	defer c.acceptWG.Done()
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		if err := c.authorize(conn); err != nil {
			_ = conn.Close()
			continue
		}

		c.connMu.Lock()
		if c.shuttingDown {
			c.connMu.Unlock()
			_ = conn.Close()
			return
		}
		c.connections[conn] = struct{}{}
		c.handlers.Add(1)
		c.connMu.Unlock()

		go func() {
			defer c.handlers.Done()
			defer func() {
				c.connMu.Lock()
				delete(c.connections, conn)
				c.connMu.Unlock()
			}()
			c.handleConn(conn)
		}()
	}
}

func (c *ControlSocket) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if _, err := conn.Write([]byte(c.handleCommand(line))); err != nil {
			return
		}
	}
}

func (c *ControlSocket) handleCommand(line string) string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "err unknown\n"
	}
	switch fields[0] {
	case "sync":
		n := c.gen.Bump()
		return fmt.Sprintf("ok generation=%d\n", n)
	default:
		return "err unknown\n"
	}
}
