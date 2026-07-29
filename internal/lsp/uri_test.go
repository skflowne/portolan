package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/skflowne/portolan/internal/core"
)

func TestWorkspaceIdentityUsesCanonicalPathCodec(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")

	path, uri, name, err := workspaceIdentity(`C:\Users\me\Project Name\..\repo`)
	if err != nil {
		t.Fatalf("workspaceIdentity: %v", err)
	}
	if path != "/mnt/c/Users/me/repo" {
		t.Errorf("path = %q, want canonical host path", path)
	}
	if uri != "file:///mnt/c/Users/me/repo" {
		t.Errorf("URI = %q, want canonical file URI", uri)
	}
	if name != "repo" {
		t.Errorf("name = %q, want repo", name)
	}
}

func TestPrepareOpenUsesCanonicalIdentityForReadCacheAndURI(t *testing.T) {
	writer := newRecordingWriteCloser()
	var reads atomic.Int32
	var readPath string
	p := newOpenUnitProvider(func(_ context.Context, path string) ([]byte, error) {
		reads.Add(1)
		readPath = path
		return []byte("export const value = 1"), nil
	}, writer)

	path, uri, err := p.prepareOpen(context.Background(), `C:\repo\space name.ts`)
	if err != nil {
		t.Fatalf("prepareOpen Windows spelling: %v", err)
	}
	if path != "/mnt/c/repo/space name.ts" || readPath != path {
		t.Fatalf("canonical path/read path = %q/%q", path, readPath)
	}
	if uri != "file:///mnt/c/repo/space%20name.ts" {
		t.Fatalf("URI = %q", uri)
	}

	path, uri, err = p.prepareOpen(context.Background(), "/mnt/C/repo/./space name.ts")
	if err != nil {
		t.Fatalf("prepareOpen equivalent spelling: %v", err)
	}
	if path != "/mnt/c/repo/space name.ts" || uri != "file:///mnt/c/repo/space%20name.ts" {
		t.Fatalf("equivalent identity = (%q, %q)", path, uri)
	}
	if reads.Load() != 1 {
		t.Fatalf("read count = %d, want one canonical open", reads.Load())
	}
	if len(p.openFiles) != 1 {
		t.Fatalf("open cache entries = %d, want one", len(p.openFiles))
	}
	if methods := writer.methods(); len(methods) != 1 || methods[0] != "textDocument/didOpen" {
		t.Fatalf("written methods = %v, want one didOpen", methods)
	}

	var notification struct {
		Params didOpenParams `json:"params"`
	}
	if err := json.Unmarshal(writer.messageBodies()[0], &notification); err != nil {
		t.Fatalf("decode didOpen: %v", err)
	}
	if got := notification.Params.TextDocument.URI; got != "file:///mnt/c/repo/space%20name.ts" {
		t.Errorf("didOpen URI = %q", got)
	}
}

func TestPrepareOpenRejectsInvalidIdentityBeforeSideEffects(t *testing.T) {
	writer := newRecordingWriteCloser()
	var reads atomic.Int32
	p := newOpenUnitProvider(func(context.Context, string) ([]byte, error) {
		reads.Add(1)
		return []byte("source"), nil
	}, writer)

	path, uri, err := p.prepareOpen(context.Background(), "relative/file.ts")
	if err == nil {
		t.Fatal("prepareOpen accepted a relative path")
	}
	if path != "" || uri != "" {
		t.Fatalf("prepareOpen returned (%q, %q) on error", path, uri)
	}
	if reads.Load() != 0 || len(writer.methods()) != 0 || len(p.openFiles) != 0 {
		t.Fatalf("invalid path side effects: reads=%d methods=%v cache=%d", reads.Load(), writer.methods(), len(p.openFiles))
	}
}

func TestDecodeLocationsUsesCanonicalURIIdentity(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	rangeJSON := `{"start":{"line":1,"character":2},"end":{"line":3,"character":4}}`
	multiline := core.Range{Start: core.Position{Line: 1, Character: 2}, End: core.Position{Line: 3, Character: 4}}
	selection := core.Range{Start: core.Position{Line: 7, Character: 1}, End: core.Position{Line: 7, Character: 2}}
	tests := []struct {
		name string
		raw  string
		want core.Location
	}{
		{"space and Unicode", `{"uri":"file://localhost/tmp/space%20%E2%98%83.ts","range":` + rangeJSON + `}`, core.Location{File: "/tmp/space ☃.ts", Range: multiline}},
		{"drive path", `{"uri":"file:///C:/Users/me/a.ts","range":` + rangeJSON + `}`, core.Location{File: "/mnt/c/Users/me/a.ts", Range: multiline}},
		{"WSL authority", `{"uri":"file://wsl.localhost/Ubuntu/home/me/a.ts","range":` + rangeJSON + `}`, core.Location{File: "/home/me/a.ts", Range: multiline}},
		{"legacy WSL authority", `{"uri":"file://wsl$/Ubuntu/home/me/a.ts","range":` + rangeJSON + `}`, core.Location{File: "/home/me/a.ts", Range: multiline}},
		{"location link selection", `{"targetUri":"file:///tmp/a.ts","targetRange":` + rangeJSON + `,"targetSelectionRange":{"start":{"line":7,"character":1},"end":{"line":7,"character":2}}}`, core.Location{File: "/tmp/a.ts", Range: selection}},
		{"location link target fallback", `{"targetUri":"file:///tmp/a.ts","targetRange":` + rangeJSON + `}`, core.Location{File: "/tmp/a.ts", Range: multiline}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			locations, err := decodeLocations(context.Background(), json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("decodeLocations: %v", err)
			}
			if len(locations) != 1 || locations[0] != tc.want {
				t.Fatalf("locations = %+v, want %+v", locations, []core.Location{tc.want})
			}
		})
	}
}

func TestDecodeLocationsRejectsUnsupportedResultURIs(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
	rng := `"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}`
	tests := []struct {
		name string
		uri  string
	}{
		{"malformed", "file:///tmp/%zz.ts"},
		{"non-file", "https://example.com/a.ts"},
		{"unsupported authority", "file://server/share/a.ts"},
		{"cross-distro WSL", "file://wsl.localhost/Debian/home/me/a.ts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(`[{"uri":` + mustJSON(t, tc.uri) + `,` + rng + `}]`)
			locations, err := decodeLocations(context.Background(), raw)
			if err == nil || locations != nil {
				t.Fatalf("decodeLocations = (%+v, %v), want explicit error", locations, err)
			}
		})
	}
}

func TestDecodeLocationLinkRejectsUnrepresentableWSLURI(t *testing.T) {
	t.Setenv("WSL_DISTRO_NAME", "")
	raw := json.RawMessage(`[{"targetUri":"file://wsl$/Ubuntu/home/me/a.ts","targetRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}]`)
	locations, err := decodeLocations(context.Background(), raw)
	if err == nil || locations != nil {
		t.Fatalf("decodeLocations = (%+v, %v), want unrepresentable URI error", locations, err)
	}
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDecodeLocationsPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	locations, err := decodeLocations(ctx, json.RawMessage(`[{"uri":"file:///tmp/a.ts","range":{"start":{},"end":{}}}]`))
	if !errors.Is(err, context.Canceled) || locations != nil {
		t.Fatalf("decodeLocations = (%+v, %v), want context cancellation", locations, err)
	}
}
