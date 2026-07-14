package artifact

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStoreBoundsAtomicArtifactsAndRejectsTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := NewFileStore(FileStoreConfig{Root: root, MaxRequestBytes: 32, MaxResultBytes: 64})
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	attemptID := strings.Repeat("a", 64)
	if err := store.WriteRequest(context.Background(), attemptID, []byte(`{"request":true}`)); err != nil {
		t.Fatalf("WriteRequest() error = %v", err)
	}
	request, err := store.ReadRequest(context.Background(), attemptID)
	if err != nil || string(request) != `{"request":true}` {
		t.Fatalf("ReadRequest() = %q, %v", request, err)
	}
	if err := store.WriteResult(context.Background(), attemptID, []byte(`{"result":true}`)); err != nil {
		t.Fatalf("WriteResult() error = %v", err)
	}
	result, err := store.ReadResult(context.Background(), attemptID)
	if err != nil || string(result) != `{"result":true}` {
		t.Fatalf("ReadResult() = %q, %v", result, err)
	}
	if err := store.WriteRequest(context.Background(), "../escape", []byte("x")); err == nil {
		t.Fatal("WriteRequest(traversal) error = nil")
	}
	if err := store.WriteRequest(context.Background(), attemptID, []byte(strings.Repeat("x", 33))); err == nil {
		t.Fatal("WriteRequest(oversize) error = nil")
	}
	entries, err := os.ReadDir(filepath.Join(root, attemptID))
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("partial artifact remained: %s", entry.Name())
		}
	}
}

func TestFileStoreCleanupIsIdempotent(t *testing.T) {
	store, err := NewFileStore(FileStoreConfig{Root: filepath.Join(t.TempDir(), "artifacts"), MaxRequestBytes: 32, MaxResultBytes: 64})
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	attemptID := strings.Repeat("b", 64)
	if err := store.WriteRequest(context.Background(), attemptID, []byte("request")); err != nil {
		t.Fatalf("WriteRequest() error = %v", err)
	}
	if err := store.Cleanup(context.Background(), attemptID); err != nil {
		t.Fatalf("Cleanup(first) error = %v", err)
	}
	if err := store.Cleanup(context.Background(), attemptID); err != nil {
		t.Fatalf("Cleanup(second) error = %v", err)
	}
}
