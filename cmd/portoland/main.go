// Command portoland is the portolan daemon: it starts a project-keyed
// control socket and serves the code-graph MCP tools over stdio.
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
	"github.com/skflowne/portolan/internal/pathnorm"
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

	projectRoot := fs.String("project-root", "", "absolute root of the analyzed project (default current working directory)")
	jsonlPath := fs.String("jsonl", "", "path to write the telemetry JSONL stream to")
	sessionID := fs.String("session-id", envOr("PORTOLAN_SESSION_ID", ""), "non-empty session id tagging every telemetry event")
	graphMode := fs.String("graph-mode", envOr("PORTOLAN_GRAPH_MODE", core.DefaultGraphMode), `eval axis: "graph" or "no-graph"`)
	controlSocket := fs.String("control-socket", "", "control-socket path (empty uses the project-keyed default)")
	tsgoPath := fs.String("tsgo", "tsgo", "tsgo executable (resolved on PATH if not absolute)")
	maxResults := fs.Int("max-results", 0, "maximum returned items; for find_references, maximum first-seen files (0 = default)")

	if err := fs.Parse(args); err != nil {
		return core.Config{}, err
	}

	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
	})
	if !setFlags["project-root"] {
		cwd, err := os.Getwd()
		if err != nil {
			return core.Config{}, fmt.Errorf("portoland: resolving --project-root default from current working directory: %w", err)
		}
		*projectRoot = cwd
	}

	canonicalRoot, err := canonicalFlagPath("project-root", *projectRoot)
	if err != nil {
		return core.Config{}, err
	}
	canonicalJSONL, err := canonicalOptionalFlagPath("jsonl", *jsonlPath)
	if err != nil {
		return core.Config{}, err
	}
	canonicalControlSocket, err := canonicalOptionalFlagPath("control-socket", *controlSocket)
	if err != nil {
		return core.Config{}, err
	}

	cfg := core.Config{
		ProjectRoot:   canonicalRoot,
		SessionID:     *sessionID,
		GraphMode:     *graphMode,
		TsgoPath:      *tsgoPath,
		JSONLPath:     canonicalJSONL,
		ControlSocket: canonicalControlSocket,
		MaxResults:    *maxResults,
	}
	if err := cfg.ValidateTelemetryDimensions(); err != nil {
		return core.Config{}, telemetryDimensionError(err, setFlags)
	}
	return cfg, nil
}

func telemetryDimensionError(err error, setFlags map[string]bool) error {
	var source string
	switch {
	case errors.Is(err, core.ErrInvalidSessionID) && setFlags["session-id"]:
		source = "--session-id"
	case errors.Is(err, core.ErrInvalidSessionID) && envIsSet("PORTOLAN_SESSION_ID"):
		source = "PORTOLAN_SESSION_ID"
	case errors.Is(err, core.ErrInvalidGraphMode) && setFlags["graph-mode"]:
		source = "--graph-mode"
	case errors.Is(err, core.ErrInvalidGraphMode) && envIsSet("PORTOLAN_GRAPH_MODE"):
		source = "PORTOLAN_GRAPH_MODE"
	default:
		source = "telemetry dimensions"
	}
	return fmt.Errorf("portoland: validating %s: %w", source, err)
}

func envIsSet(key string) bool {
	_, ok := os.LookupEnv(key)
	return ok
}

func canonicalOptionalFlagPath(name, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	return canonicalFlagPath(name, value)
}

func canonicalFlagPath(name, value string) (string, error) {
	canonical, err := pathnorm.Canonicalize(value)
	if err != nil {
		return "", fmt.Errorf("portoland: validating --%s %q: %w", name, value, err)
	}
	return canonical, nil
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
