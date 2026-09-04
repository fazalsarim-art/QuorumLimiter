package storage

import (
	"path/filepath"
	"sync"
	"testing"
)

func TestTermAndVoteReopenRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quorumlimiter.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.SetTermAndVote(7, "node2"); err != nil {
		t.Fatalf("SetTermAndVote: %v", err)
	}
	if err := st.AppendEntries([]LogEntry{
		{Index: 1, Term: 7, Kind: KindNoop},
		{Index: 2, Term: 7, Kind: KindCommand, Command: []byte(`{"x":1}`)},
	}); err != nil {
		t.Fatalf("AppendEntries: %v", err)
	}
	if err := st.SetCommitIndex(2); err != nil {
		t.Fatalf("SetCommitIndex: %v", err)
	}
	if err := st.SetLastApplied(1); err != nil {
		t.Fatalf("SetLastApplied: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and confirm everything persisted exactly.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st2.Close() }()

	if term, _ := st2.CurrentTerm(); term != 7 {
		t.Errorf("term = %d, want 7", term)
	}
	if vote, _ := st2.VotedFor(); vote != "node2" {
		t.Errorf("votedFor = %q, want node2", vote)
	}
	if ci, _ := st2.CommitIndex(); ci != 2 {
		t.Errorf("commitIndex = %d, want 2", ci)
	}
	if la, _ := st2.LastApplied(); la != 1 {
		t.Errorf("lastApplied = %d, want 1", la)
	}
	e, ok, err := st2.Entry(2)
	if err != nil || !ok {
		t.Fatalf("Entry(2): ok=%v err=%v", ok, err)
	}
	if e.Term != 7 || e.Kind != KindCommand || string(e.Command) != `{"x":1}` {
		t.Errorf("entry 2 = %+v, not recovered exactly", e)
	}
}

func TestVoteDefaultsEmpty(t *testing.T) {
	st := newTestStore(t)
	if vote, _ := st.VotedFor(); vote != "" {
		t.Errorf("fresh votedFor = %q, want empty", vote)
	}
	if term, _ := st.CurrentTerm(); term != 0 {
		t.Errorf("fresh term = %d, want 0", term)
	}
}

func TestLogOrderingFirstLastAndRange(t *testing.T) {
	st := newTestStore(t)
	// Append out of insertion convenience but with ascending indexes.
	var entries []LogEntry
	for i := uint64(1); i <= 4; i++ {
		entries = append(entries, LogEntry{Index: i, Term: 1, Kind: KindCommand})
	}
	if err := st.AppendEntries(entries); err != nil {
		t.Fatalf("AppendEntries: %v", err)
	}

	if fi, _ := st.FirstIndex(); fi != 0 {
		t.Errorf("FirstIndex = %d, want 0 (sentinel)", fi)
	}
	if li, _ := st.LastIndex(); li != 4 {
		t.Errorf("LastIndex = %d, want 4", li)
	}

	got, err := st.Entries(1, 4)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("Entries returned %d, want 4", len(got))
	}
	for i, e := range got {
		if e.Index != uint64(i+1) {
			t.Errorf("Entries[%d].Index = %d, want %d (out of order)", i, e.Index, i+1)
		}
	}
}

func TestTruncateSuffix(t *testing.T) {
	st := newTestStore(t)
	var entries []LogEntry
	for i := uint64(1); i <= 4; i++ {
		entries = append(entries, LogEntry{Index: i, Term: 1, Kind: KindCommand})
	}
	if err := st.AppendEntries(entries); err != nil {
		t.Fatalf("AppendEntries: %v", err)
	}

	if err := st.TruncateSuffix(3); err != nil {
		t.Fatalf("TruncateSuffix: %v", err)
	}

	if _, ok, _ := st.Entry(2); !ok {
		t.Error("entry 2 should remain after truncate from 3")
	}
	if _, ok, _ := st.Entry(3); ok {
		t.Error("entry 3 should be gone after truncate from 3")
	}
	if _, ok, _ := st.Entry(4); ok {
		t.Error("entry 4 should be gone after truncate from 3")
	}
	if li, _ := st.LastIndex(); li != 2 {
		t.Errorf("LastIndex after truncate = %d, want 2", li)
	}
	// The sentinel must survive even an aggressive truncate.
	if err := st.TruncateSuffix(0); err != nil {
		t.Fatalf("TruncateSuffix(0): %v", err)
	}
	if _, ok, _ := st.Entry(0); !ok {
		t.Error("sentinel must never be truncated")
	}
}

func TestEntryMissing(t *testing.T) {
	st := newTestStore(t)
	if _, ok, err := st.Entry(99); err != nil || ok {
		t.Errorf("Entry(99): ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}

func TestConcurrentReads(t *testing.T) {
	st := newTestStore(t)
	if err := st.SetCurrentTerm(3); err != nil {
		t.Fatalf("SetCurrentTerm: %v", err)
	}
	if err := st.AppendEntries([]LogEntry{{Index: 1, Term: 3, Kind: KindNoop}}); err != nil {
		t.Fatalf("AppendEntries: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if term, err := st.CurrentTerm(); err != nil || term != 3 {
					t.Errorf("CurrentTerm concurrent read = %d err=%v", term, err)
					return
				}
				if _, err := st.LastIndex(); err != nil {
					t.Errorf("LastIndex concurrent read err=%v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
