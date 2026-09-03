package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestMetricsRecordsEventsWithBoundedLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, "node1")

	m.ObserveDecision("allowed")
	m.ObserveDecision("allowed")
	m.ObserveDecision("denied")
	m.IncElection()
	m.IncRPC("append_entries", "ok")

	out := scrape(t, m)
	if !strings.Contains(out, `quorumlimiter_decisions_total{node="node1",result="allowed"} 2`) {
		t.Errorf("missing allowed decisions count:\n%s", out)
	}
	if !strings.Contains(out, `quorumlimiter_decisions_total{node="node1",result="denied"} 1`) {
		t.Errorf("missing denied decisions count")
	}
	if !strings.Contains(out, `quorumlimiter_raft_elections_total{node="node1"} 1`) {
		t.Errorf("missing elections count")
	}
	if !strings.Contains(out, `quorumlimiter_raft_rpc_total{node="node1",result="ok",rpc="append_entries"} 1`) {
		t.Errorf("missing rpc count:\n%s", out)
	}
}

func TestMetricsMiddlewareBoundedRoutes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, "node1")

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/decisions", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	h := m.Middleware(mux)

	// Many distinct high-cardinality paths must all fold into "other".
	for _, p := range []string{"/x/1", "/x/2", "/x/3"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, p, nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/decisions", nil))

	out := scrape(t, m)
	if !strings.Contains(out, `route="other"`) {
		t.Errorf("expected bounded 'other' route label:\n%s", out)
	}
	if strings.Contains(out, `route="/x/1"`) {
		t.Error("raw path leaked into a metric label")
	}
	if !strings.Contains(out, `quorumlimiter_http_requests_total{method="POST",node="node1",route="/v1/decisions",status_class="2xx"} 1`) {
		t.Errorf("missing decision request metric:\n%s", out)
	}
}

func TestMetricsCollectorSetsGauges(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg, "node1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := func() (Snapshot, error) {
		return Snapshot{Role: "leader", Term: 7, CommitIndex: 10, LastApplied: 8, LastLogIndex: 10}, nil
	}
	m.StartCollector(ctx, snap, func() int64 { return 32768 }, 10*time.Millisecond)

	// Poll the scrape until the collector has run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out := scrape(t, m)
		if strings.Contains(out, `quorumlimiter_raft_role{node="node1",role="leader"} 1`) &&
			strings.Contains(out, `quorumlimiter_raft_apply_lag{node="node1"} 2`) &&
			strings.Contains(out, `quorumlimiter_bbolt_size_bytes{node="node1"} 32768`) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("collector did not set expected gauges:\n%s", scrape(t, m))
}

func TestMetricsNoDuplicateRegistrationPanic(t *testing.T) {
	// Each call uses its own registry, so building twice must not panic.
	New(prometheus.NewRegistry(), "node1")
	New(prometheus.NewRegistry(), "node2")
}
