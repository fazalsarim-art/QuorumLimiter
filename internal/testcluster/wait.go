package testcluster

import (
	"testing"
	"time"
)

// pollInterval is how often Eventually re-checks its condition. This is bounded
// polling, not a fixed sleep: the condition is checked until it holds or the
// deadline passes.
const pollInterval = 5 * time.Millisecond

// Eventually polls cond until it returns true or timeout elapses, failing the
// test with msg (and any diagnostics from diag) on timeout.
func Eventually(t *testing.T, timeout time.Duration, cond func() bool, msg string, diag func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(pollInterval)
	}
	if diag != nil {
		t.Fatalf("%s\ndiagnostics:\n%s", msg, diag())
	}
	t.Fatal(msg)
}
