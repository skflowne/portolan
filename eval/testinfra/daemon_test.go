package testinfra

import (
	"strings"
	"testing"
)

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
