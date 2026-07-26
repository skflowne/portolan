package tiera

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/skflowne/portolan/eval/testinfra"
	"github.com/skflowne/portolan/internal/tools"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

type otlpReceiver struct {
	server   *httptest.Server
	requests chan *collectortracepb.ExportTraceServiceRequest
}

func newOTLPReceiver(t *testing.T) *otlpReceiver {
	t.Helper()
	receiver := &otlpReceiver{requests: make(chan *collectortracepb.ExportTraceServiceRequest, 8)}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading OTLP request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request collectortracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			t.Errorf("decoding OTLP request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		receiver.requests <- &request
		response, _ := proto.Marshal(&collectortracepb.ExportTraceServiceResponse{})
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(response)
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

func startTelemetryDaemon(t *testing.T, endpoint string) (*testinfra.Daemon, *mcp.ClientSession, string) {
	t.Helper()
	dir := t.TempDir()
	jsonl := filepath.Join(dir, "telemetry.jsonl")
	d := testinfra.NewDaemon(t, testinfra.Config{
		Binary:        daemonBin,
		ProjectRoot:   testinfra.FixtureRoot(),
		Telemetry:     jsonl,
		SessionID:     "telemetry-integration",
		ControlSocket: filepath.Join(dir, "control.sock"),
		Env: map[string]string{
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": endpoint,
		},
	})
	sess := testinfra.ConnectMCP(t, d, "telemetry-integration")
	d.WaitForPID(t)
	return d, sess, jsonl
}

func callOneTelemetryTool(t *testing.T, sess *mcp.ClientSession) {
	t.Helper()
	var out tools.GetOutlineOutput
	callInto(t, sess, "get_outline", map[string]any{
		"file": filepath.Join(testinfra.FixtureRoot(), "src", "geometry.ts"),
	}, &out)
	if !out.Found {
		t.Fatalf("expected outline result: %+v", out)
	}
}

func stopTelemetryDaemon(t *testing.T, d *testinfra.Daemon, sess *mcp.ClientSession) {
	t.Helper()
	_ = sess.Close()
	_ = d.Stdin.Close()
	if err, ok := d.WaitForExit(5 * time.Second); !ok {
		t.Fatal("daemon did not exit within telemetry shutdown bound")
	} else if err != nil {
		t.Fatalf("daemon exit: %v (stderr=%s)", err, d.Stderr())
	}
	_, _ = io.ReadAll(d.Stdout)
}

func assertProtocolOnlyStdout(t *testing.T, raw []byte) {
	t.Helper()
	if len(bytes.TrimSpace(raw)) == 0 {
		t.Fatal("captured daemon stdout is empty")
	}
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var message map[string]any
		if err := json.Unmarshal(line, &message); err != nil {
			t.Fatalf("non-JSON MCP stdout line %q: %v", line, err)
		}
		if message["jsonrpc"] != "2.0" {
			t.Fatalf("non-MCP stdout message: %s", line)
		}
	}
}

func readExactTelemetryEvent(t *testing.T, path string) map[string]any {
	t.Helper()
	lines := readJSONL(t, path)
	if len(lines) != 1 {
		t.Fatalf("JSONL records = %d, want exactly 1: %v", len(lines), lines)
	}
	return lines[0]
}

func spanAttributes(attributes []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(attributes))
	for _, attr := range attributes {
		if attr.Value != nil {
			out[attr.Key] = attr.Value.GetStringValue()
		}
	}
	return out
}

func TestDaemonOTLPHTTPPreservesJSONLAndMCPStdout(t *testing.T) {
	receiver := newOTLPReceiver(t)
	d, sess, jsonl := startTelemetryDaemon(t, receiver.server.URL+"/v1/traces")
	callOneTelemetryTool(t, sess)
	stopTelemetryDaemon(t, d, sess)

	event := readExactTelemetryEvent(t, jsonl)
	if event["tool"] != "get_outline" || event["session_id"] != "telemetry-integration" || event["graph_mode"] != "graph" {
		t.Fatalf("wrong JSONL event: %v", event)
	}
	select {
	case request := <-receiver.requests:
		var spans int
		var attrs map[string]string
		for _, resource := range request.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				for _, span := range scope.Spans {
					spans++
					attrs = spanAttributes(span.Attributes)
				}
			}
		}
		if spans != 1 || attrs["tool"] != event["tool"] || attrs["session_id"] != event["session_id"] || attrs["graph_mode"] != event["graph_mode"] || attrs["ts"] != event["ts"] {
			t.Fatalf("OTLP span does not match JSONL event: spans=%d attrs=%v event=%v", spans, attrs, event)
		}
	case <-time.After(time.Second):
		t.Fatal("fake OTLP collector received no span")
	}
	assertProtocolOnlyStdout(t, d.StdoutBytes())
}

func TestDaemonStalledOTLPShutdownIsBoundedAndJSONLComplete(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	d, sess, jsonl := startTelemetryDaemon(t, server.URL+"/v1/traces")
	callOneTelemetryTool(t, sess)
	_ = sess.Close()
	_ = d.Stdin.Close()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("stalled OTLP request did not start")
	}
	started := time.Now()
	if err, ok := d.WaitForExit(5 * time.Second); !ok {
		t.Fatal("daemon hung on stalled OTLP collector")
	} else if err != nil {
		t.Fatalf("daemon failed during bounded OTLP shutdown: %v (stderr=%s)", err, d.Stderr())
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("daemon OTLP shutdown took %s", elapsed)
	}
	_, _ = io.ReadAll(d.Stdout)
	readExactTelemetryEvent(t, jsonl)
	assertProtocolOnlyStdout(t, d.StdoutBytes())
	if strings.Contains(d.StdoutString(), "traceId") {
		t.Fatalf("stdout contains console-exported span: %s", d.StdoutString())
	}
}
