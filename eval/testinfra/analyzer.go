package testinfra

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const (
	// RequiredTsgoVersion pins the analyzer that authors the Tier A contract.
	RequiredTsgoVersion = "Version 7.0.0-dev.20260707.2"
	RequireTsgoEnv      = "PORTOLAN_REQUIRE_TSGO"
)

func resolveTsgo() (string, error) {
	return tsgoStatus(exec.LookPath, tsgoVersion)
}

func tsgoStatus(
	lookup func(string) (string, error),
	version func(string) (string, error),
) (string, error) {
	path, err := lookup("tsgo")
	if err != nil {
		return "", fmt.Errorf("tsgo %s: executable not found on PATH: %w", RequiredTsgoVersion, err)
	}
	got, err := version(path)
	if err != nil {
		return "", fmt.Errorf("checking tsgo version: %w", err)
	}
	got = strings.TrimSpace(got)
	if got != RequiredTsgoVersion {
		return "", fmt.Errorf("incompatible tsgo version: got %q, want %q", got, RequiredTsgoVersion)
	}
	return path, nil
}

func tsgoVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ShortWait)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s --version: %w", path, ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("%s --version: %w (%s)", path, err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
