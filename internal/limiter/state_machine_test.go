package limiter

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/fazalsarim-art/QuorumLimiter/internal/storage"
)

// --- test harness ---

type harness struct {
	t   *testing.T
	sm  *StateMachine
	st  *storage.Store
	idx uint64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "n", "db")
	st, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &harness{t: t, sm: New(), st: st}
}

// apply builds a command, applies it at the next log index, and returns the
// result. It fails the test on an infrastructure error.
func (h *harness) apply(typ CommandType, tsMS int64, payload any) Result {
	h.t.Helper()
	h.idx++
	cmd, err := NewCommand(fmt.Sprintf("cmd-%d", h.idx), typ, tsMS, payload)
	if err != nil {
		h.t.Fatalf("build command: %v", err)
	}
	res, err := h.sm.Apply(h.st, ApplyContext{LogIndex: h.idx, Term: 1}, cmd)
	if err != nil {
		h.t.Fatalf("apply %s: %v", typ, err)
	}
	return res
}

func (h *harness) seedPolicy(id string, capacity, refill, interval, maxCost int64) {
	h.t.Helper()
	r := h.apply(CmdCreatePolicy, 1000, CreatePolicyPayload{
		ID: id, Name: "Policy " + id, CapacityTokens: capacity,
		RefillTokens: refill, RefillIntervalMS: interval, MaxCostTokens: maxCost, Active: true,
	})
	if r.Outcome != OutcomeApplied {
		h.t.Fatalf("seedPolicy: outcome %s (%s)", r.Outcome, r.RejectMsg)
	}
}

func (h *harness) seedClient(id string, allowed []string) {
	h.t.Helper()
	r := h.apply(CmdCreateClient, 1000, CreateClientPayload{
		ID: id, Name: "Client " + id, KeyPrefix: "PFX" + id, KeyDigest: []byte("digest-" + id),
		AllowedPolicyIDs: allowed,
	})
	if r.Outcome != OutcomeApplied {
		h.t.Fatalf("seedClient: outcome %s (%s)", r.Outcome, r.RejectMsg)
	}
}

func decide(clientID, policyID, subject string, cost int64, reqID string) DecidePayload {
	return DecidePayload{ClientID: clientID, PolicyID: policyID, Subject: subject, Cost: cost, RequestID: reqID}
}

// --- headline scenario from the guide ---

func TestTokenBucketFiveThenDenyThenRefill(t *testing.T) {
	h := newHarness(t)
	h.seedPolicy("password_reset", 5, 1, 12000, 5)
	h.seedClient("cli_1", []string{"password_reset"})

	// Five immediate cost-1 requests at t=1000 are all allowed.
	for i := 0; i < 5; i++ {
		r := h.apply(CmdDecide, 1000, decide("cli_1", "password_reset", "customer_4821", 1, fmt.Sprintf("req-%016d", i)))
		if r.Outcome != OutcomeAllowed {
			t.Fatalf("request %d: outcome %s, want allowed", i, r.Outcome)
		}
	}
	// The sixth immediate request is denied.
	r6 := h.apply(CmdDecide, 1000, decide("cli_1", "password_reset", "customer_4821", 1, "req-denied-000000"))
	if r6.Outcome != OutcomeDenied {
		t.Fatalf("sixth request outcome %s, want denied", r6.Outcome)
	}
	if r6.Decision.RetryAfterMS <= 0 {
		t.Errorf("denied retry_after = %d, want positive", r6.Decision.RetryAfterMS)
	}

	// 12s later one token has refilled: one more cost-1 is allowed.
	r7 := h.apply(CmdDecide, 13000, decide("cli_1", "password_reset", "customer_4821", 1, "req-after-12s-0001"))
	if r7.Outcome != OutcomeAllowed {
		t.Fatalf("after 12s outcome %s, want allowed", r7.Outcome)
	}
}

// --- idempotency ---

func TestIdempotentReplayReturnsOriginal(t *testing.T) {
	h := newHarness(t)
	h.seedPolicy("pol", 5, 1, 12000, 5)
	h.seedClient("cli_1", []string{"*"})

	first := h.apply(CmdDecide, 1000, decide("cli_1", "pol", "s", 2, "req-idem-00000001"))
	if first.Outcome != OutcomeAllowed || first.Duplicate {
		t.Fatalf("first: outcome %s dup %v", first.Outcome, first.Duplicate)
	}
	remainingAfterFirst := first.Decision.RemainingMilli

	// Replaying the same request id returns the original result unchanged and
	// does not deduct again.
	replay := h.apply(CmdDecide, 5000, decide("cli_1", "pol", "s", 2, "req-idem-00000001"))
	if !replay.Duplicate {
		t.Error("replay not marked duplicate")
	}
	if replay.Decision.RemainingMilli != remainingAfterFirst {
		t.Errorf("replay remaining = %d, want original %d", replay.Decision.RemainingMilli, remainingAfterFirst)
	}

	// A fresh request should now see the balance reduced by only the first spend.
	fresh := h.apply(CmdDecide, 1000, decide("cli_1", "pol", "s", 1, "req-fresh-00000002"))
	if fresh.Decision.RemainingMilli != remainingAfterFirst-1000 {
		t.Errorf("balance after replay = %d, want %d (replay double-charged?)",
			fresh.Decision.RemainingMilli, remainingAfterFirst-1000)
	}
}

func TestIdempotencyConflict(t *testing.T) {
	h := newHarness(t)
	h.seedPolicy("pol", 5, 1, 12000, 5)
	h.seedClient("cli_1", []string{"*"})

	h.apply(CmdDecide, 1000, decide("cli_1", "pol", "s", 1, "req-conflict-000001"))
	// Same key, different content -> conflict.
	r := h.apply(CmdDecide, 1000, decide("cli_1", "pol", "s", 3, "req-conflict-000001"))
	if r.Outcome != OutcomeRejected || r.RejectCode != RejectIdempotencyConflict {
		t.Fatalf("conflict: outcome %s code %s", r.Outcome, r.RejectCode)
	}
}

// --- decision rejections ---

func TestDecideRejections(t *testing.T) {
	h := newHarness(t)
	h.seedPolicy("active_p", 5, 1, 12000, 3)
	h.seedPolicy("inactive_p", 5, 1, 12000, 3)
	h.apply(CmdSetPolicyActive, 1000, SetPolicyActivePayload{ID: "inactive_p", Active: false, ExpectedVersion: 1})
	h.seedClient("cli_scoped", []string{"active_p"})
	h.seedClient("cli_all", []string{"*"})
	h.apply(CmdCreateClient, 1000, CreateClientPayload{
		ID: "cli_revoked", Name: "Revoked", KeyPrefix: "PFXrev", KeyDigest: []byte("d"),
		AllowedPolicyIDs: []string{"*"},
	})
	h.apply(CmdRevokeClient, 1000, RevokeClientPayload{ID: "cli_revoked"})

	cases := []struct {
		name    string
		payload DecidePayload
		code    string
	}{
		{"unknown policy", decide("cli_all", "no_such", "s", 1, "req-unknownpol-0001"), RejectPolicyNotFound},
		{"inactive policy", decide("cli_all", "inactive_p", "s", 1, "req-inactive-00001"), RejectPolicyInactive},
		{"unknown client", decide("ghost", "active_p", "s", 1, "req-ghostcli-00001"), RejectClientNotFound},
		{"revoked client", decide("cli_revoked", "active_p", "s", 1, "req-revoked-000001"), RejectClientRevoked},
		{"forbidden policy", decide("cli_scoped", "inactive_p", "s", 1, "req-forbidden-0001"), RejectForbiddenPolicy},
		{"cost exceeds max", decide("cli_all", "active_p", "s", 4, "req-costmax-000001"), RejectValidation},
		{"bad subject", decide("cli_all", "active_p", "bad subject!", 1, "req-badsubj-00001"), RejectValidation},
		{"short request id", decide("cli_all", "active_p", "s", 1, "short"), RejectValidation},
		{"zero cost", decide("cli_all", "active_p", "s", 0, "req-zerocost-00001"), RejectValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := h.apply(CmdDecide, 2000, tc.payload)
			if r.Outcome != OutcomeRejected || r.RejectCode != tc.code {
				t.Fatalf("outcome %s code %q, want rejected/%s", r.Outcome, r.RejectCode, tc.code)
			}
		})
	}
}

// --- policy lifecycle ---

func TestPolicyCreateDuplicateAndActivation(t *testing.T) {
	h := newHarness(t)
	h.seedPolicy("pol", 5, 1, 12000, 5)

	dup := h.apply(CmdCreatePolicy, 1000, CreatePolicyPayload{
		ID: "pol", Name: "dup", CapacityTokens: 5, RefillTokens: 1, RefillIntervalMS: 12000, MaxCostTokens: 5, Active: true,
	})
	if dup.RejectCode != RejectPolicyExists {
		t.Fatalf("duplicate policy: %s", dup.RejectCode)
	}

	// Activation with a stale expected version conflicts.
	stale := h.apply(CmdSetPolicyActive, 1000, SetPolicyActivePayload{ID: "pol", Active: false, ExpectedVersion: 99})
	if stale.RejectCode != RejectVersionConflict {
		t.Fatalf("stale activation: %s", stale.RejectCode)
	}
	ok := h.apply(CmdSetPolicyActive, 1000, SetPolicyActivePayload{ID: "pol", Active: false, ExpectedVersion: 1})
	if ok.Outcome != OutcomeApplied || ok.Policy.Active {
		t.Fatalf("activation: outcome %s active %v", ok.Outcome, ok.Policy.Active)
	}
	if ok.Policy.Version != 2 {
		t.Errorf("version after activation = %d, want 2", ok.Policy.Version)
	}
}

func TestPolicyUpdateConvertsBucketsAtUpdateTime(t *testing.T) {
	h := newHarness(t)
	// capacity 10, refill 1 token / 1000ms.
	h.seedPolicy("pol", 10, 1, 1000, 5)
	h.seedClient("cli", []string{"*"})

	// Spend to drain: 5 cost-1 at t=1000 -> 5 tokens (5000 milli) left.
	for i := 0; i < 5; i++ {
		h.apply(CmdDecide, 1000, decide("cli", "pol", "s", 1, fmt.Sprintf("req-drain-%08d", i)))
	}

	// At t=6000 (5s later), update: reduce capacity to 6, keep refill 1/1000.
	// Old-rate credit for 5s = 5 tokens; from 5 -> 10, clamped to new capacity 6.
	upd := h.apply(CmdUpdatePolicy, 6000, UpdatePolicyPayload{
		ID: "pol", Name: "Updated policy", CapacityTokens: 6, RefillTokens: 1, RefillIntervalMS: 1000, MaxCostTokens: 5, ExpectedVersion: 1,
	})
	if upd.Outcome != OutcomeApplied || upd.Policy.Version != 2 {
		t.Fatalf("update: outcome %s version %d", upd.Outcome, upd.Policy.Version)
	}

	// A decision at the same instant (t=6000) should see the clamped balance:
	// 6 tokens = 6000 milli, minus this cost 1 -> 5000 remaining.
	r := h.apply(CmdDecide, 6000, decide("cli", "pol", "s", 1, "req-postupdate-0001"))
	if r.Outcome != OutcomeAllowed {
		t.Fatalf("post-update decide outcome %s", r.Outcome)
	}
	if r.Decision.RemainingMilli != 5000 {
		t.Errorf("remaining = %d, want 5000 (bucket not clamped to new capacity at update time)", r.Decision.RemainingMilli)
	}
}

// --- client lifecycle ---

func TestClientRevokeIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.seedClient("cli", []string{"*"})
	first := h.apply(CmdRevokeClient, 1000, RevokeClientPayload{ID: "cli"})
	if first.Outcome != OutcomeApplied || first.Client.Active {
		t.Fatalf("revoke: outcome %s active %v", first.Outcome, first.Client.Active)
	}
	second := h.apply(CmdRevokeClient, 2000, RevokeClientPayload{ID: "cli"})
	if second.Outcome != OutcomeApplied || second.Client.Active {
		t.Fatalf("re-revoke should be idempotent success: %s", second.Outcome)
	}
}

// --- pruning ---

func TestPruneExpiredIdempotency(t *testing.T) {
	h := newHarness(t)
	h.seedPolicy("pol", 100, 1, 1000, 5)
	h.seedClient("cli", []string{"*"})

	// A decision at t=1000 expires 24h later.
	h.apply(CmdDecide, 1000, decide("cli", "pol", "s", 1, "req-prune-00000001"))

	// Prune well after expiry removes it.
	pr := h.apply(CmdPrune, 1000+idempotencyTTLms+1, PrunePayload{})
	if pr.Prune.IdempotencyRemoved != 1 {
		t.Fatalf("pruned %d idempotency records, want 1", pr.Prune.IdempotencyRemoved)
	}

	// Replaying the pruned request now runs fresh (not a duplicate).
	replay := h.apply(CmdDecide, 1000+idempotencyTTLms+2, decide("cli", "pol", "s", 1, "req-prune-00000001"))
	if replay.Duplicate {
		t.Error("request should not be a duplicate after pruning")
	}
}

// --- determinism: two fresh stores, same commands, identical state ---

func TestDeterministicConvergence(t *testing.T) {
	build := func() *storage.Store {
		h := newHarness(t)
		h.seedPolicy("pol", 20, 3, 5000, 10)
		h.seedClient("cli", []string{"*"})
		subjects := []string{"a", "b", "customer_4821", "d"}
		for i := 0; i < 200; i++ {
			subj := subjects[i%len(subjects)]
			ts := int64(1000 + i*137)
			cost := int64(1 + i%5)
			h.apply(CmdDecide, ts, decide("cli", "pol", subj, cost, fmt.Sprintf("req-conv-%08d", i)))
		}
		return h.st
	}

	dump1, err := build().DebugDump()
	if err != nil {
		t.Fatalf("dump1: %v", err)
	}
	dump2, err := build().DebugDump()
	if err != nil {
		t.Fatalf("dump2: %v", err)
	}
	if string(dump1) != string(dump2) {
		t.Error("two stores diverged after identical command sequence")
	}
}

// --- randomized reference model (fixed seed) ---

// refBucket is an independent reimplementation of the bucket math used to
// cross-check the state machine's decisions.
type refBucket struct {
	tokensMilli, lastRefill, remainder int64
}

func refDecide(b *refBucket, capacity, refill, interval, cost, now int64) (bool, int64) {
	capMilli := capacity * 1000
	eff := b.lastRefill
	if now > eff {
		eff = now
	}
	elapsed := eff - b.lastRefill
	numerator := elapsed*refill*1000 + b.remainder
	added := numerator / interval
	rem := numerator % interval
	nt := b.tokensMilli + added
	if nt >= capMilli {
		b.tokensMilli = capMilli
		b.remainder = 0
	} else {
		b.tokensMilli = nt
		b.remainder = rem
	}
	b.lastRefill = eff

	costMilli := cost * 1000
	if b.tokensMilli >= costMilli {
		b.tokensMilli -= costMilli
		return true, b.tokensMilli
	}
	return false, b.tokensMilli
}

func TestRandomizedMatchesReferenceModel(t *testing.T) {
	const (
		capacity = int64(50)
		refill   = int64(2)
		interval = int64(1000)
		maxCost  = int64(5)
	)
	h := newHarness(t)
	h.seedPolicy("pol", capacity, refill, interval, maxCost)
	h.seedClient("cli", []string{"*"})

	rng := rand.New(rand.NewSource(42)) // fixed seed -> deterministic test
	subjects := []string{"alpha", "beta", "gamma"}
	refs := map[string]*refBucket{}
	for _, s := range subjects {
		refs[s] = &refBucket{tokensMilli: capacity * 1000, lastRefill: 0, remainder: 0}
	}

	now := int64(0)
	for i := 0; i < 500; i++ {
		now += int64(rng.Intn(1500))
		subj := subjects[rng.Intn(len(subjects))]
		cost := int64(1 + rng.Intn(int(maxCost)))

		// The reference bucket is initialized at lastRefill=0 like a first-use
		// bucket that started full at t=0; align the state machine by using the
		// same starting assumption (first decision seeds the bucket full).
		wantAllowed, wantRemaining := refDecide(refs[subj], capacity, refill, interval, cost, now)

		r := h.apply(CmdDecide, now, decide("cli", "pol", subj, cost, fmt.Sprintf("req-rnd-%08d", i)))
		gotAllowed := r.Outcome == OutcomeAllowed
		if gotAllowed != wantAllowed {
			t.Fatalf("iter %d subj %s cost %d now %d: allowed=%v want %v", i, subj, cost, now, gotAllowed, wantAllowed)
		}
		if r.Decision.RemainingMilli != wantRemaining {
			t.Fatalf("iter %d subj %s: remaining=%d want %d", i, subj, r.Decision.RemainingMilli, wantRemaining)
		}
	}
}
