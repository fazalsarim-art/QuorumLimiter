package storage

import (
	"path/filepath"
	"testing"
)

// newTestStore opens a Store in a fresh temp directory whose parent does not yet
// exist, exercising directory creation. It is closed automatically.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	// The "node" subdirectory does not exist yet, so Open must create it.
	path := filepath.Join(t.TempDir(), "node", "quorumlimiter.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
