package testinfra

import (
	"errors"
	"strings"
	"testing"
)

func TestTsgoStatusRequiresPinnedVersion(t *testing.T) {
	tests := []struct {
		name        string
		lookupPath  string
		lookupErr   error
		version     string
		versionErr  error
		wantPath    string
		wantErrText string
	}{
		{name: "pinned", lookupPath: "/bin/tsgo", version: RequiredTsgoVersion + "\n", wantPath: "/bin/tsgo"},
		{name: "missing", lookupErr: errors.New("not found"), wantErrText: "executable not found on PATH"},
		{name: "version command", lookupPath: "/bin/tsgo", versionErr: errors.New("exit 1"), wantErrText: "checking tsgo version"},
		{name: "incompatible", lookupPath: "/bin/tsgo", version: "Version 7.0.0-dev.other\n", wantErrText: "incompatible tsgo version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tsgoStatus(
				func(string) (string, error) { return tt.lookupPath, tt.lookupErr },
				func(string) (string, error) { return tt.version, tt.versionErr },
			)
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil || got != tt.wantPath {
				t.Fatalf("tsgoStatus = %q, %v, want %q, nil", got, err, tt.wantPath)
			}
		})
	}
}

func TestControlledDaemonEnvOmitsAndOverridesKeys(t *testing.T) {
	t.Setenv("PORTOLAN_TEST_PRESERVED", "preserved")
	t.Setenv("PORTOLAN_TEST_OMITTED", "ambient")
	t.Setenv("PORTOLAN_TEST_OVERRIDDEN", "ambient")

	env := controlledDaemonEnv(
		map[string]string{"PORTOLAN_TEST_OVERRIDDEN": "configured"},
		"PORTOLAN_TEST_OMITTED",
	)
	got := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		got[key] = value
	}

	if _, ok := got["PORTOLAN_TEST_OMITTED"]; ok {
		t.Fatal("omitted environment variable was retained")
	}
	if got["PORTOLAN_TEST_OVERRIDDEN"] != "configured" {
		t.Fatalf("overridden environment variable = %q, want configured", got["PORTOLAN_TEST_OVERRIDDEN"])
	}
	if got["PORTOLAN_TEST_PRESERVED"] != "preserved" {
		t.Fatalf("preserved environment variable = %q, want preserved", got["PORTOLAN_TEST_PRESERVED"])
	}
}
