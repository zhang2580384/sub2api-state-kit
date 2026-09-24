package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type diagnosticStatus struct {
	DiagnosticsEnabled   bool              `json:"diagnostics_enabled"`
	DiagnosticsListening bool              `json:"diagnostics_listening"`
	DiagnosticsRetained  int               `json:"diagnostics_retained"`
	Diagnostics          []diagnosticEvent `json:"diagnostics"`
}

func diagnosticHealth(t *testing.T, e *Engine) diagnosticStatus {
	t.Helper()
	response, err := e.Health(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var status diagnosticStatus
	if err := json.Unmarshal([]byte(response.StatusJson), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestDiagnosticHistoryIsPersistentBoundedAndRedacted(t *testing.T) {
	e := newEngine(nil, "https://example.invalid", time.Second)
	defer e.Close()

	rawState := testState(292)
	e.recordDiagnostic(diagnosticEvent{Stage: "capture", Outcome: "accepted", StateClass: stateDiagnosticClass(rawState),
		TargetProxy: "socks5://proxy-user:proxy-secret@proxy.example:1080", CaptureEgress: "203.0.113.18"})
	if status := diagnosticHealth(t, e); !status.DiagnosticsEnabled || status.DiagnosticsListening || len(status.Diagnostics) != 1 {
		t.Fatalf("diagnostic history was not retained before the panel opened: %+v", status)
	}

	started := time.Now()
	e.mu.Lock()
	e.observeDiagnosticPollLocked(started)
	e.observeDiagnosticPollLocked(started.Add(400 * time.Millisecond))
	e.observeDiagnosticPollLocked(started.Add(800 * time.Millisecond))
	e.mu.Unlock()
	for i := 0; i < maxDiagnosticEvents+10; i++ {
		e.recordDiagnostic(diagnosticEvent{Stage: "capture", Outcome: "accepted", StateClass: stateDiagnosticClass(rawState),
			TargetProxy: "socks5://proxy-user:proxy-secret@proxy.example:1080", CaptureEgress: "203.0.113.18"})
	}
	response, err := e.Health(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	status := diagnosticHealth(t, e)
	if !status.DiagnosticsEnabled {
		t.Fatal("diagnostic listener was not reported")
	}
	if !status.DiagnosticsListening {
		t.Fatal("diagnostic listener compatibility flag was not reported")
	}
	if len(status.Diagnostics) != maxDiagnosticResponseEvents || status.DiagnosticsRetained != maxDiagnosticEvents {
		t.Fatalf("diagnostic response length/retained = %d/%d, want %d/%d",
			len(status.Diagnostics), status.DiagnosticsRetained, maxDiagnosticResponseEvents, maxDiagnosticEvents)
	}
	wantLastSeq := uint64(maxDiagnosticEvents + 11)
	wantFirstSeq := wantLastSeq - uint64(maxDiagnosticResponseEvents) + 1
	if status.Diagnostics[0].Seq != wantFirstSeq || status.Diagnostics[len(status.Diagnostics)-1].Seq != wantLastSeq {
		t.Fatalf("unexpected retained sequence range: %d..%d", status.Diagnostics[0].Seq, status.Diagnostics[len(status.Diagnostics)-1].Seq)
	}
	if response.StatusJson == "" || strings.Contains(response.StatusJson, "proxy-secret") || strings.Contains(response.StatusJson, rawState) {
		t.Fatalf("diagnostic status leaked sensitive data: %s", response.StatusJson)
	}
	if !strings.Contains(response.StatusJson, "socks5://proxy.example:1080") {
		t.Fatalf("redacted proxy endpoint missing: %s", response.StatusJson)
	}

	expired := started.Add(20 * time.Second)
	e.mu.Lock()
	e.observeDiagnosticPollLocked(expired)
	statusAfterClose := e.snapshotLocked(expired)
	e.mu.Unlock()
	if statusAfterClose.DiagnosticsListening || len(statusAfterClose.Diagnostics) != maxDiagnosticResponseEvents ||
		statusAfterClose.DiagnosticsRetained != maxDiagnosticEvents {
		t.Fatalf("diagnostic history was not retained after the UI listener expired: %+v", statusAfterClose)
	}
}

func TestDiagnosticHistoryExpiresAfterFiveHours(t *testing.T) {
	e := newEngine(nil, "https://example.invalid", time.Second)
	defer e.Close()

	now := time.Now().UTC()
	e.diagnostics = []diagnosticEvent{
		{Seq: 1, Timestamp: now.Add(-maxDiagnosticAge - time.Minute).Format(time.RFC3339Nano), Stage: "capture", Outcome: "probe_failed"},
		{Seq: 2, Timestamp: now.Add(-maxDiagnosticAge + time.Minute).Format(time.RFC3339Nano), Stage: "capture", Outcome: "accepted"},
	}
	e.mu.Lock()
	e.pruneDiagnosticsLocked(now)
	status := e.snapshotLocked(now)
	e.mu.Unlock()
	if len(status.Diagnostics) != 1 || status.Diagnostics[0].Seq != 2 {
		t.Fatalf("five-hour diagnostic pruning retained %+v", status.Diagnostics)
	}
}

func TestDiagnosticListenerIgnoresNormalOneSecondHostHealthChecks(t *testing.T) {
	e := newEngine(nil, "https://example.invalid", time.Second)
	defer e.Close()

	started := time.Now()
	e.mu.Lock()
	for i := 0; i < 4; i++ {
		e.observeDiagnosticPollLocked(started.Add(time.Duration(i) * time.Second))
	}
	active := e.diagnosticsListeningLocked(started.Add(3 * time.Second))
	e.mu.Unlock()
	if active {
		t.Fatal("normal host health checks incorrectly opened the diagnostics listener")
	}

	e.mu.Lock()
	burst := started.Add(5 * time.Second)
	for i := 0; i < diagnosticBurstCount; i++ {
		e.observeDiagnosticPollLocked(burst.Add(time.Duration(i) * 300 * time.Millisecond))
	}
	active = e.diagnosticsListeningLocked(burst.Add(600 * time.Millisecond))
	e.mu.Unlock()
	if !active {
		t.Fatal("UI status burst did not open the diagnostics listener")
	}
}

func TestDiagnosticStateAndEgressClassification(t *testing.T) {
	if got := stateDiagnosticClass(testState(292)); got != "292" {
		t.Fatalf("292 classification = %q", got)
	}
	if got := stateDiagnosticClass(testState(312)); got != "312" {
		t.Fatalf("312 classification = %q", got)
	}
	if got := stateDiagnosticClass(testState(780)); got != "780" {
		t.Fatalf("780 classification = %q", got)
	}
	if got := stateDiagnosticClass(""); got != "empty" {
		t.Fatalf("empty classification = %q", got)
	}
	if got := stateDiagnosticClass("opaque"); got != "other" {
		t.Fatalf("other classification = %q", got)
	}
	match := egressMatch("203.0.113.18", "203.0.113.18")
	if match == nil || !*match {
		t.Fatal("equal egress IPs did not match")
	}
	mismatch := egressMatch("203.0.113.18", "198.51.100.24")
	if mismatch == nil || *mismatch {
		t.Fatal("different egress IPs incorrectly matched")
	}
	if egressMatch("", "198.51.100.24") != nil {
		t.Fatal("unknown egress should not be reported as a match")
	}
	if safeEgressIP("not-an-ip") != "" {
		t.Fatal("invalid egress value was accepted")
	}
}
