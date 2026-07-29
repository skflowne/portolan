package core

import (
	"strings"
	"testing"
)

func TestConfigValidateTelemetryDimensions(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
		graphMode string
		wantError string
	}{
		{name: "graph", sessionID: "session-1", graphMode: "graph"},
		{name: "no graph", sessionID: "session-1", graphMode: "no-graph"},
		{name: "empty session", graphMode: "graph", wantError: "session ID"},
		{name: "whitespace session", sessionID: " \t\n", graphMode: "graph", wantError: "session ID"},
		{name: "empty graph mode", sessionID: "session-1", wantError: "graph mode"},
		{name: "unknown graph mode", sessionID: "session-1", graphMode: "enabled", wantError: "graph mode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Config{SessionID: tt.sessionID, GraphMode: tt.graphMode}).ValidateTelemetryDimensions()
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateTelemetryDimensions: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ValidateTelemetryDimensions error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

func TestDefaultGraphModeIsValid(t *testing.T) {
	if DefaultGraphMode != "graph" {
		t.Fatalf("DefaultGraphMode = %q, want graph", DefaultGraphMode)
	}
	if err := (Config{SessionID: "session-1", GraphMode: DefaultGraphMode}).ValidateTelemetryDimensions(); err != nil {
		t.Fatalf("default graph mode is invalid: %v", err)
	}
}
