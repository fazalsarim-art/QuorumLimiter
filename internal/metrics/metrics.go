// Package metrics defines the Prometheus collectors and the HTTP middleware,
// event hooks, and periodic collector that drive them. Labels are deliberately
// bounded — never an API key, request id, subject, client id, or raw path — to
// keep cardinality safe.
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Snapshot is the consensus state the periodic collector reads. It is a plain
// struct so this package does not import raft (avoiding an import cycle).
type Snapshot struct {
	Role                string
	Term                uint64
	CommitIndex         uint64
	LastApplied         uint64
	LastLogIndex        uint64
	LastQuorumContactMS int64
}

// Metrics holds all collectors registered on one registry.
type Metrics struct {
	reg    *prometheus.Registry
	nodeID string

	httpRequests     *prometheus.CounterVec
	httpDuration     *prometheus.HistogramVec
	decisions        *prometheus.CounterVec
	elections        prometheus.Counter
	rpcs             *prometheus.CounterVec
	proposalDuration prometheus.Histogram

	role             *prometheus.GaugeVec
	term             prometheus.Gauge
	commitIndex      prometheus.Gauge
	lastApplied      prometheus.Gauge
	lastLogIndex     prometheus.Gauge
	applyLag         prometheus.Gauge
	dbSizeBytes      prometheus.Gauge
	quorumContactAge prometheus.Gauge
}

// New builds and registers the collectors on reg. A per-node constant label
// keeps "node" bounded to one value.
func New(reg *prometheus.Registry, nodeID string) *Metrics {
	cl := prometheus.Labels{"node": nodeID}
	m := &Metrics{reg: reg, nodeID: nodeID}

	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "quorumlimiter_http_requests_total", Help: "HTTP requests by route, method, and status class.", ConstLabels: cl,
	}, []string{"route", "method", "status_class"})
	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "quorumlimiter_http_request_duration_seconds", Help: "HTTP request duration.", ConstLabels: cl,
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})
	m.decisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "quorumlimiter_decisions_total", Help: "Decision outcomes.", ConstLabels: cl,
	}, []string{"result"})
	m.elections = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "quorumlimiter_raft_elections_total", Help: "Elections this node has started.", ConstLabels: cl,
	})
	m.rpcs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "quorumlimiter_raft_rpc_total", Help: "Outbound Raft RPCs by type and result.", ConstLabels: cl,
	}, []string{"rpc", "result"})
	m.proposalDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "quorumlimiter_raft_proposal_duration_seconds", Help: "Time from proposal to committed apply.", ConstLabels: cl,
		Buckets: prometheus.DefBuckets,
	})
	m.role = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "quorumlimiter_raft_role", Help: "Current role (1 for the active role).", ConstLabels: cl,
	}, []string{"role"})
	m.term = newGauge("quorumlimiter_raft_term", "Current Raft term.", cl)
	m.commitIndex = newGauge("quorumlimiter_raft_commit_index", "Commit index.", cl)
	m.lastApplied = newGauge("quorumlimiter_raft_last_applied_index", "Last applied index.", cl)
	m.lastLogIndex = newGauge("quorumlimiter_raft_last_log_index", "Last log index.", cl)
	m.applyLag = newGauge("quorumlimiter_raft_apply_lag", "commit_index - last_applied.", cl)
	m.dbSizeBytes = newGauge("quorumlimiter_bbolt_size_bytes", "bbolt database size in bytes.", cl)
	m.quorumContactAge = newGauge("quorumlimiter_raft_last_quorum_contact_seconds", "Seconds since last quorum contact (leader).", cl)

	reg.MustRegister(
		m.httpRequests, m.httpDuration, m.decisions, m.elections, m.rpcs, m.proposalDuration,
		m.role, m.term, m.commitIndex, m.lastApplied, m.lastLogIndex, m.applyLag, m.dbSizeBytes, m.quorumContactAge,
	)
	return m
}

func newGauge(name, help string, cl prometheus.Labels) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help, ConstLabels: cl})
}

// Handler returns the /metrics HTTP handler for this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// --- request middleware ---

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Middleware records request count and duration with bounded labels.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		route := classifyRoute(r.URL.Path)
		m.httpRequests.WithLabelValues(route, r.Method, statusClass(sw.status)).Inc()
		m.httpDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}

// classifyRoute maps a path to one of a small, bounded set of route templates so
// per-path cardinality never explodes.
func classifyRoute(path string) string {
	switch {
	case path == "/v1/decisions":
		return "/v1/decisions"
	case path == "/health/live":
		return "/health/live"
	case path == "/health/ready":
		return "/health/ready"
	case path == "/metrics":
		return "/metrics"
	case strings.HasPrefix(path, "/internal/raft/"):
		return "/internal/raft"
	case strings.HasPrefix(path, "/api/admin/"):
		return "/api/admin"
	case strings.HasPrefix(path, "/admin/static/"):
		return "/admin/static"
	case strings.HasPrefix(path, "/admin"):
		return "/admin"
	default:
		return "other"
	}
}

func statusClass(code int) string {
	return strconv.Itoa(code/100) + "xx"
}

// --- event hooks (implement raft.Metrics) ---

// ObserveDecision records a decision outcome ("allowed", "denied", "rejected").
func (m *Metrics) ObserveDecision(result string) { m.decisions.WithLabelValues(result).Inc() }

// IncElection counts an election this node started.
func (m *Metrics) IncElection() { m.elections.Inc() }

// IncRPC counts an outbound Raft RPC ("request_vote"/"append_entries",
// "ok"/"error").
func (m *Metrics) IncRPC(rpc, result string) { m.rpcs.WithLabelValues(rpc, result).Inc() }

// ObserveProposalSeconds records proposal-to-apply latency.
func (m *Metrics) ObserveProposalSeconds(seconds float64) { m.proposalDuration.Observe(seconds) }

// --- periodic collector ---

// StartCollector periodically refreshes the state gauges until ctx is cancelled.
func (m *Metrics) StartCollector(ctx context.Context, snapshot func() (Snapshot, error), dbSize func() int64, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		m.collect(snapshot, dbSize)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.collect(snapshot, dbSize)
			}
		}
	}()
}

func (m *Metrics) collect(snapshot func() (Snapshot, error), dbSize func() int64) {
	s, err := snapshot()
	if err != nil {
		return
	}
	for _, role := range []string{"follower", "candidate", "leader"} {
		v := 0.0
		if role == s.Role {
			v = 1
		}
		m.role.WithLabelValues(role).Set(v)
	}
	m.term.Set(float64(s.Term))
	m.commitIndex.Set(float64(s.CommitIndex))
	m.lastApplied.Set(float64(s.LastApplied))
	m.lastLogIndex.Set(float64(s.LastLogIndex))
	m.applyLag.Set(float64(s.CommitIndex - s.LastApplied))
	if dbSize != nil {
		m.dbSizeBytes.Set(float64(dbSize()))
	}
	if s.LastQuorumContactMS > 0 {
		m.quorumContactAge.Set(time.Since(time.UnixMilli(s.LastQuorumContactMS)).Seconds())
	}
}
