// Command portoland is the portolan daemon: it starts a project-keyed
// control socket (Phase 0 scaffold for the Phase 1 staleness barrier) and
// serves the three code-graph MCP tools (find_definition, find_references,
// get_outline) over stdio.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/skflowne/portolan/internal/core"
	"github.com/skflowne/portolan/internal/lsp"
	pmcp "github.com/skflowne/portolan/internal/mcp"
	"github.com/skflowne/portolan/internal/telemetry"
	"github.com/skflowne/portolan/internal/tools"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	if cfg.JSONLPath == "" {
		cfg.JSONLPath = defaultJSONLPath(cfg.ProjectRoot)
	}
	logger, err := telemetry.FromConfig(cfg, func(err error) {
		log.Printf("portoland: telemetry: %v", err)
	})
	if err != nil {
		cancel()
		return fmt.Errorf("portoland: opening telemetry stream: %w", err)
	}

	// The real LSP provider (tsgo --lsp) — spawns the subprocess and completes
	// the initialize handshake.
	provider, err := lsp.New(cfg)
	if err != nil {
		cleanupErr := logger.Close()
		cancel()
		return joinStartupErrors(
			fmt.Errorf("portoland: starting language provider: %w", err),
			cleanupError{"closing telemetry", cleanupErr},
		)
	}

	gen := &core.GenerationCounter{}
	t := tools.New(provider, gen, logger, cfg)

	sockPath := pmcp.SocketPath(cfg)
	control := pmcp.NewControlSocket(sockPath, gen)
	if err := control.Start(ctx); err != nil {
		providerErr := provider.Close()
		loggerErr := logger.Close()
		cancel()
		return joinStartupErrors(
			fmt.Errorf("portoland: starting control socket: %w", err),
			cleanupError{"closing provider", providerErr},
			cleanupError{"closing telemetry", loggerErr},
		)
	}
	log.Printf("portoland: control socket listening on %s", sockPath)

	srv := pmcp.NewServer(t)
	log.Printf("portoland: serving MCP over stdio (project_root=%s session_id=%s graph_mode=%s)",
		cfg.ProjectRoot, cfg.SessionID, cfg.GraphMode)

	runErr := pmcp.RunStdio(ctx, srv)
	wasCanceled := ctx.Err() != nil
	// RunStdio can return on stdin EOF without cancelling its context. Cancel
	// explicitly before waiting for the control listener and its connections.
	cancel()
	control.Wait()
	if err := provider.Close(); err != nil {
		log.Printf("portoland: provider close: %v", err)
	}
	if err := logger.Close(); err != nil {
		log.Printf("portoland: logger close: %v", err)
	}

	if runErr != nil {
		// Context cancellation (SIGINT/SIGTERM) surfaces here as an error from
		// the underlying transport; treat it as a clean shutdown rather than
		// a failure.
		if wasCanceled || errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			return nil
		}
		return fmt.Errorf("portoland: mcp server: %w", runErr)
	}
	return nil
}

// parseConfig builds a core.Config from flags and environment. Flags take
// precedence over environment variables, which take precedence over
// defaults.
func parseConfig(args []string) (core.Config, error) {
	return parseConfigWithOutput(args, os.Stderr)
}

func parseConfigWithOutput(args []string, output io.Writer) (core.Config, error) {
	fs := flag.NewFlagSet("portoland", flag.ContinueOnError)
	fs.SetOutput(output)

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	projectRoot := fs.String("project-root", cwd, "absolute root of the analyzed project")
	jsonlPath := fs.String("jsonl", "", "path to write the telemetry JSONL stream to")
	sessionID := fs.String("session-id", envOr("PORTOLAN_SESSION_ID", ""), "session id tagging every telemetry event")
	graphMode := fs.String("graph-mode", envOr("PORTOLAN_GRAPH_MODE", "graph"), `eval axis: "graph" or "no-graph"`)
	controlSocket := fs.String("control-socket", "", "control-socket path (empty uses the project-keyed default)")
	tsgoPath := fs.String("tsgo", "tsgo", "tsgo executable (resolved on PATH if not absolute)")
	maxResults := fs.Int("max-results", 0, "cap applied to every list-returning tool result (0 = default)")

	if err := fs.Parse(args); err != nil {
		return core.Config{}, err
	}

	abs, err := filepath.Abs(*projectRoot)
	if err != nil {
		return core.Config{}, fmt.Errorf("portoland: resolving --project-root %q: %w", *projectRoot, err)
	}

	return core.Config{
		ProjectRoot:   abs,
		SessionID:     *sessionID,
		GraphMode:     *graphMode,
		TsgoPath:      *tsgoPath,
		JSONLPath:     *jsonlPath,
		ControlSocket: *controlSocket,
		MaxResults:    *maxResults,
	}, nil
}

type cleanupError struct {
	label string
	err   error
}

func joinStartupErrors(primary error, cleanup ...cleanupError) error {
	errs := []error{primary}
	for _, item := range cleanup {
		if item.err != nil {
			errs = append(errs, fmt.Errorf("portoland: %s: %w", item.label, item.err))
		}
	}
	return errors.Join(errs...)
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// defaultJSONLPath returns a project-keyed telemetry path under the temp dir,
// so that logging is always on even when --jsonl is not supplied and two
// daemons for different projects don't clobber each other's stream.
func defaultJSONLPath(projectRoot string) string {
	sum := sha256.Sum256([]byte(projectRoot))
	return filepath.Join(os.TempDir(), fmt.Sprintf("portoland-%s.jsonl", hex.EncodeToString(sum[:6])))
}
