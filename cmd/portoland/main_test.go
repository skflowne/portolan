package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pmcp "github.com/skflowne/portolan/internal/mcp"
)

func TestJoinStartupErrorsPreservesPrimaryAndCleanupFailures(t *testing.T) {
	primary := errors.New("provider start failed")
	providerClose := errors.New("provider close failed")
	telemetryClose := errors.New("telemetry close failed")
	err := joinStartupErrors(
		primary,
		cleanupError{"closing provider", providerClose},
		cleanupError{"closing telemetry", telemetryClose},
	)
	for _, want := range []error{primary, providerClose, telemetryClose} {
		if !errors.Is(err, want) {
			t.Fatalf("joined error %v does not preserve %v", err, want)
		}
	}
	for _, label := range []string{"closing provider", "closing telemetry"} {
		if !strings.Contains(err.Error(), label) {
			t.Fatalf("joined error %q misses %q", err, label)
		}
	}
}

func TestParseConfigHelpDescribesControlSocketDefault(t *testing.T) {
	var output bytes.Buffer
	_, err := parseConfigWithOutput([]string{"-h"}, &output)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parseConfigWithOutput(-h) error = %v, want flag.ErrHelp", err)
	}

	help := output.String()
	start := strings.Index(help, "-control-socket")
	if start == -1 {
		t.Fatalf("help does not contain control-socket flag:\n%s", help)
	}
	entry := help[start:]
	if end := strings.Index(entry[1:], "\n  -"); end != -1 {
		entry = entry[:end+1]
	}
	if strings.Contains(entry, "under /tmp") {
		t.Fatalf("control-socket help contains stale default:\n%s", entry)
	}
	if !strings.Contains(entry, "project-keyed default") {
		t.Fatalf("control-socket help does not delegate to the owned default:\n%s", entry)
	}
}

func TestParseConfigCanonicalizesProjectIdentityAndDerivedKeys(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	const wantRoot = "/mnt/c/Users/me/repo"
	digest := sha256.Sum256([]byte(wantRoot))
	wantJSONL := filepath.Join(tempDir, fmt.Sprintf("portoland-%s.jsonl", hex.EncodeToString(digest[:6])))
	wantSocket := filepath.Join(runtimeDir, "portoland", fmt.Sprintf("portoland-%s.sock", hex.EncodeToString(digest[:])[:12]))

	for _, input := range []string{
		`C:\Users\me\repo`,
		`/mnt/c/Users/me/repo`,
		`\\wsl$\Ubuntu\mnt\c\Users\me\repo`,
		`\\wsl.localhost\Ubuntu\mnt\c\Users\me\repo`,
	} {
		cfg, err := parseConfigWithOutput([]string{"--project-root", input}, io.Discard)
		if err != nil {
			t.Fatalf("parseConfigWithOutput(--project-root %q): %v", input, err)
		}
		if cfg.ProjectRoot != wantRoot {
			t.Errorf("ProjectRoot for %q = %q, want %q", input, cfg.ProjectRoot, wantRoot)
		}
		if got := defaultJSONLPath(cfg.ProjectRoot); got != wantJSONL {
			t.Errorf("defaultJSONLPath for %q = %q, want %q", input, got, wantJSONL)
		}
		if got := pmcp.SocketPath(cfg); got != wantSocket {
			t.Errorf("SocketPath for %q = %q, want %q", input, got, wantSocket)
		}
	}

	other, err := parseConfigWithOutput([]string{"--project-root", "/mnt/d/Users/me/repo"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfigWithOutput(second identity): %v", err)
	}
	if defaultJSONLPath(other.ProjectRoot) == wantJSONL {
		t.Error("distinct canonical projects produced the same default JSONL path")
	}
	if pmcp.SocketPath(other) == wantSocket {
		t.Error("distinct canonical projects produced the same control socket path")
	}
}

func TestParseConfigDefaultsProjectRootToCanonicalWorkingDirectory(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := t.TempDir()
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir(%q): %v", root, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	cfg, err := parseConfigWithOutput(nil, io.Discard)
	if err != nil {
		t.Fatalf("parseConfigWithOutput: %v", err)
	}
	if cfg.ProjectRoot != filepath.Clean(root) {
		t.Errorf("ProjectRoot = %q, want current directory %q", cfg.ProjectRoot, filepath.Clean(root))
	}
}

func TestParseConfigReportsUnavailableDefaultWorkingDirectory(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root, err := os.MkdirTemp("", "portolan-removed-cwd-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir(%q): %v", root, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
	if err := os.Remove(root); err != nil {
		t.Fatalf("Remove(%q): %v", root, err)
	}

	_, err = parseConfigWithOutput(nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--project-root") {
		t.Fatalf("parseConfigWithOutput error = %v, want --project-root working-directory error", err)
	}
}

func TestParseConfigRejectsInvalidProjectRoots(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	for _, input := range []string{
		"",
		"relative/repo",
		`\\server\share\repo`,
		`//server/share/repo`,
		`\\wsl$\Debian\home\me\repo`,
		"/home/me/repo\x00bad",
	} {
		_, err := parseConfigWithOutput([]string{"--project-root", input}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--project-root") {
			t.Errorf("parseConfigWithOutput(--project-root %q) error = %v, want flag-specific error", input, err)
		}
	}
}

func TestParseConfigCanonicalizesExplicitOperationalPaths(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	const want = "/mnt/c/Users/me/portolan/state"
	inputs := []string{
		`C:\Users\me\portolan\state`,
		`/mnt/C/Users/me/portolan/state`,
		`\\wsl$\Ubuntu\mnt\c\Users\me\portolan\state`,
		`\\wsl.localhost\Ubuntu\mnt\c\Users\me\portolan\state`,
	}
	for _, flagName := range []string{"jsonl", "control-socket"} {
		for _, input := range inputs {
			cfg, err := parseConfigWithOutput([]string{"--project-root", "/repo", "--" + flagName, input}, io.Discard)
			if err != nil {
				t.Fatalf("parseConfigWithOutput(--%s %q): %v", flagName, input, err)
			}
			var got string
			if flagName == "jsonl" {
				got = cfg.JSONLPath
			} else {
				got = cfg.ControlSocket
			}
			if got != want {
				t.Errorf("--%s %q = %q, want %q", flagName, input, got, want)
			}
		}
	}
}

func TestParseConfigRejectsInvalidExplicitOperationalPaths(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	for _, flagName := range []string{"jsonl", "control-socket"} {
		for _, input := range []string{
			"relative/path",
			`\\server\share\state`,
			`\\wsl.localhost\Debian\home\me\state`,
			"/tmp/state\x00bad",
		} {
			_, err := parseConfigWithOutput([]string{"--project-root", "/repo", "--" + flagName, input}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "--"+flagName) {
				t.Errorf("parseConfigWithOutput(--%s %q) error = %v, want flag-specific error", flagName, input, err)
			}
		}
	}
}

func TestParseConfigPreservesEmptyOptionalPathDefaults(t *testing.T) {
	cfg, err := parseConfigWithOutput([]string{
		"--project-root", "/repo",
		"--jsonl=",
		"--control-socket=",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfigWithOutput: %v", err)
	}
	if cfg.JSONLPath != "" || cfg.ControlSocket != "" {
		t.Fatalf("optional paths = (%q, %q), want both empty", cfg.JSONLPath, cfg.ControlSocket)
	}
}
