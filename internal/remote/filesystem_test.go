package remote

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/zeebo/blake3"
)

func TestWriteAtomicPublishesAndOverwritesBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.json")

	if err := WriteAtomic(path, []byte("first")); err != nil {
		t.Fatalf("WriteAtomic(first) error = %v", err)
	}
	if err := WriteAtomic(path, []byte("second")); err != nil {
		t.Fatalf("WriteAtomic(second) error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "second" {
		t.Fatalf("ReadFile() = %q, %v; want second, nil", contents, err)
	}
}

func TestWriteAtomicReturnsLocalStorageError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "atomic.json")

	if err := WriteAtomic(path, []byte("contents")); domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("WriteAtomic() error = %v; want local storage", err)
	}
}

func TestFilesystemPublishesObjectsAndCASRefs(t *testing.T) {
	ctx := context.Background()
	transport, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	contents := []byte(`{"schema_version":2}`)
	digest := blake3.Sum256(contents)
	id := fmt.Sprintf("%x", digest[:])
	published, err := transport.PublishObject(ctx, id, contents)
	if err != nil || !published {
		t.Fatalf("PublishObject() = %t, %v; want true, nil", published, err)
	}
	published, err = transport.PublishObject(ctx, id, contents)
	if err != nil || published {
		t.Fatalf("PublishObject(existing) = %t, %v; want false, nil", published, err)
	}
	got, err := transport.ReadObject(ctx, id)
	if err != nil || string(got) != string(contents) {
		t.Fatalf("ReadObject() = %q, %v; want %q, nil", got, err, contents)
	}
	refName := "refs/contexts/heads/main"
	empty, err := transport.ReadRef(ctx, refName)
	if err != nil || empty != (Ref{RefName: refName}) {
		t.Fatalf("ReadRef(empty) = %+v, %v", empty, err)
	}
	next := Ref{RefName: refName, CommitID: id, SourceSHA: "source", Version: 1}
	stored, err := transport.CompareAndSwapRef(ctx, refName, empty, next)
	if err != nil || stored != next {
		t.Fatalf("CompareAndSwapRef() = %+v, %v; want %+v, nil", stored, err, next)
	}
	if _, err := transport.CompareAndSwapRef(ctx, refName, empty, next); domain.CodeOf(err) != domain.CodeRemoteConflict {
		t.Fatalf("stale CompareAndSwapRef() error = %v, want remote conflict", err)
	}
}

func TestFilesystemReadObjectReturnsTypedMissingError(t *testing.T) {
	transport, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	id, _ := testObject("never published")

	if _, err := transport.ReadObject(t.Context(), id); domain.CodeOf(err) != domain.CodeObjectMissing {
		t.Fatalf("ReadObject(missing) error = %v, want %q", err, domain.CodeObjectMissing)
	}
}
