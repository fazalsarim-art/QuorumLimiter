package storage

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestOpenCreatesSchemaAndBuckets(t *testing.T) {
	st := newTestStore(t)

	v, err := st.StoredSchemaVersion()
	if err != nil {
		t.Fatalf("StoredSchemaVersion: %v", err)
	}
	if v != SchemaVersion {
		t.Errorf("schema version = %d, want %d", v, SchemaVersion)
	}

	// Every declared bucket must exist.
	err = st.db.View(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if tx.Bucket(name) == nil {
				t.Errorf("bucket %q missing after migration", name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}

	// The index-0 sentinel must be present.
	e, ok, err := st.Entry(0)
	if err != nil || !ok {
		t.Fatalf("sentinel Entry(0): ok=%v err=%v", ok, err)
	}
	if e.Kind != KindSentinel || e.Index != 0 || e.Term != 0 {
		t.Errorf("sentinel = %+v, want index 0 term 0 kind sentinel", e)
	}
}

func TestMigrationIdempotentAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quorumlimiter.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Seed a value we can verify survives a no-op second migration.
	if err := st.SetCurrentTerm(9); err != nil {
		t.Fatalf("SetCurrentTerm: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: migrate runs again but must make no changes.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	v, _ := st2.StoredSchemaVersion()
	if v != SchemaVersion {
		t.Errorf("schema version after reopen = %d, want %d", v, SchemaVersion)
	}
	term, _ := st2.CurrentTerm()
	if term != 9 {
		t.Errorf("term after reopen = %d, want 9 (data lost across reopen)", term)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quorumlimiter.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Force a stored schema version newer than this binary supports.
	err = st.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(metaSchemaVersion, u64(SchemaVersion+1))
	})
	if err != nil {
		t.Fatalf("bump schema: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("expected Open to refuse a newer schema, got nil")
	}
}

func TestOpenFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permission bits are not enforced on Windows")
	}
	st := newTestStore(t)
	info, err := os.Stat(st.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != dbFileMode {
		t.Errorf("db file mode = %o, want %o", perm, dbFileMode)
	}
}

func TestCompositeKeyUnambiguous(t *testing.T) {
	// ("a","bc") and ("ab","c") must not collide.
	k1, err := compositeKey([]byte("a"), []byte("bc"))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := compositeKey([]byte("ab"), []byte("c"))
	if err != nil {
		t.Fatal(err)
	}
	if string(k1) == string(k2) {
		t.Errorf("composite keys collided: %x == %x", k1, k2)
	}

	// Over-long parts are rejected.
	if _, err := compositeKey(make([]byte, maxKeyPartLen+1)); err == nil {
		t.Error("expected error for over-long key part, got nil")
	}
}

func TestU64RoundTripAndOrdering(t *testing.T) {
	for _, v := range []uint64{0, 1, 255, 256, 1 << 20, 1<<63 + 7} {
		if got := parseU64(u64(v)); got != v {
			t.Errorf("round trip %d = %d", v, got)
		}
	}
	// Big-endian keys sort numerically: bytes(2) < bytes(10).
	if string(u64(2)) >= string(u64(10)) {
		t.Error("u64 keys do not sort numerically")
	}
}
