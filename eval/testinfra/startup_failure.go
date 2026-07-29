package testinfra

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// StartupFailureConfig describes a daemon invocation that must fail before
// creating the listed runtime artifacts.
type StartupFailureConfig struct {
	Binary      string
	Args        []string
	Env         map[string]string
	OmitEnv     []string
	WantError   string
	AbsentPaths []string
}

// AssertDaemonStartupFailure runs a daemon through the controlled eval
// environment and verifies that configuration rejection creates no runtime artifacts.
func AssertDaemonStartupFailure(t *testing.T, cfg StartupFailureConfig) {
	t.Helper()
	RequireSupport(t)
	ctx, cancel := context.WithTimeout(context.Background(), ShortWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.Binary, cfg.Args...)
	cmd.Env = controlledDaemonEnv(cfg.Env, cfg.OmitEnv...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("daemon startup did not fail within %s: %s", ShortWait, output)
	}
	if err == nil {
		t.Fatalf("daemon unexpectedly accepted invalid startup configuration: %s", output)
	}
	if !strings.Contains(string(output), cfg.WantError) {
		t.Fatalf("startup error %q does not identify %s", output, cfg.WantError)
	}
	for _, path := range cfg.AbsentPaths {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Errorf("runtime artifact %s exists after configuration rejection: %v", path, statErr)
		}
	}
}
