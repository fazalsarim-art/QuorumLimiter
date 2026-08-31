package storage

import (
	"errors"
	"testing"
)

func TestApplyWritesAndReads(t *testing.T) {
	st := newTestStore(t)

	err := st.Apply(func(tx *StateTx) error {
		if err := tx.PutPolicy("password_reset", []byte(`{"id":"password_reset"}`)); err != nil {
			return err
		}
		if err := tx.PutClient("cli_1", []byte(`{"id":"cli_1"}`)); err != nil {
			return err
		}
		if err := tx.PutClientPrefix("K4N9Q2PT", "cli_1"); err != nil {
			return err
		}
		if err := tx.PutTokenBucket("password_reset", "customer_4821", []byte(`{"tokens_milli":4000}`)); err != nil {
			return err
		}
		if err := tx.PutIdempotency("cli_1", "req_1", []byte(`{"allowed":true}`)); err != nil {
			return err
		}
		if err := tx.PutAudit(1, []byte(`{"event":"decision"}`)); err != nil {
			return err
		}
		return tx.SetLastApplied(1)
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if v, ok, _ := st.Policy("password_reset"); !ok || string(v) != `{"id":"password_reset"}` {
		t.Errorf("Policy = %q ok=%v", v, ok)
	}
	if id, ok, _ := st.ClientIDByPrefix("K4N9Q2PT"); !ok || id != "cli_1" {
		t.Errorf("ClientIDByPrefix = %q ok=%v", id, ok)
	}
	if v, ok, _ := st.TokenBucket("password_reset", "customer_4821"); !ok || string(v) != `{"tokens_milli":4000}` {
		t.Errorf("TokenBucket = %q ok=%v", v, ok)
	}
	if v, ok, _ := st.Idempotency("cli_1", "req_1"); !ok || string(v) != `{"allowed":true}` {
		t.Errorf("Idempotency = %q ok=%v", v, ok)
	}
	if la, _ := st.LastApplied(); la != 1 {
		t.Errorf("LastApplied = %d, want 1", la)
	}
	audits, _ := st.RecentAudits(10)
	if len(audits) != 1 || string(audits[0]) != `{"event":"decision"}` {
		t.Errorf("RecentAudits = %q", audits)
	}
}

func TestApplyRollsBackOnError(t *testing.T) {
	st := newTestStore(t)

	sentinel := errors.New("boom")
	err := st.Apply(func(tx *StateTx) error {
		if err := tx.PutPolicy("p1", []byte(`{"id":"p1"}`)); err != nil {
			return err
		}
		if err := tx.SetLastApplied(5); err != nil {
			return err
		}
		return sentinel // force rollback after writes
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Apply err = %v, want sentinel", err)
	}

	// Neither the policy nor the checkpoint may survive a rolled-back tx.
	if _, ok, _ := st.Policy("p1"); ok {
		t.Error("policy persisted despite rollback")
	}
	if la, _ := st.LastApplied(); la != 0 {
		t.Errorf("LastApplied = %d, want 0 after rollback", la)
	}
}

func TestMissingApplicationKeys(t *testing.T) {
	st := newTestStore(t)
	if _, ok, _ := st.Policy("nope"); ok {
		t.Error("Policy(nope) should not be found")
	}
	if _, ok, _ := st.Client("nope"); ok {
		t.Error("Client(nope) should not be found")
	}
	if _, ok, _ := st.TokenBucket("p", "s"); ok {
		t.Error("TokenBucket(p,s) should not be found")
	}
	if audits, err := st.RecentAudits(10); err != nil || len(audits) != 0 {
		t.Errorf("RecentAudits on empty = %v err=%v", audits, err)
	}
}

func TestRecentAuditsNewestFirst(t *testing.T) {
	st := newTestStore(t)
	err := st.Apply(func(tx *StateTx) error {
		for i := uint64(1); i <= 5; i++ {
			if err := tx.PutAudit(i, []byte{byte('0' + i)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := st.RecentAudits(3)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Newest first: 5, 4, 3.
	want := []byte{'5', '4', '3'}
	for i, b := range want {
		if got[i][0] != b {
			t.Errorf("audit[%d] = %c, want %c", i, got[i][0], b)
		}
	}
}
