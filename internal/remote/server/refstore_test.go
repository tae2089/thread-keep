package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
)

const testRefNameForStore = "refs/contexts/main"

func openTestRefStore(t *testing.T, path string) *GormRefStore {
	t.Helper()
	store, err := OpenGormRefStore(path)
	if err != nil {
		t.Fatalf("OpenGormRefStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestGormRefStoreCompareAndSwapLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "refs.db")
	store := openTestRefStore(t, path)
	commitID, _ := testObject("gorm ref target")

	ref, err := store.ReadRef(ctx, "repo-1", testRefNameForStore)
	if err != nil || ref != (remote.Ref{RefName: testRefNameForStore}) {
		t.Fatalf("ReadRef(absent) = %+v, %v, want zero-version ref", ref, err)
	}

	first := remote.Ref{RefName: testRefNameForStore, CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 1}
	confirmed, err := store.CompareAndSwapRef(ctx, "repo-1", testRefNameForStore, remote.Ref{RefName: testRefNameForStore}, first)
	if err != nil || confirmed != first {
		t.Fatalf("CompareAndSwapRef(first) = %+v, %v, want confirmed ref", confirmed, err)
	}

	if _, err := store.CompareAndSwapRef(ctx, "repo-1", testRefNameForStore, remote.Ref{RefName: testRefNameForStore}, first); domain.CodeOf(err) != domain.CodeRemoteConflict {
		t.Fatalf("CompareAndSwapRef(stale expected) error = %v, want %q", err, domain.CodeRemoteConflict)
	}

	skipped := remote.Ref{RefName: testRefNameForStore, CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 3}
	if _, err := store.CompareAndSwapRef(ctx, "repo-1", testRefNameForStore, first, skipped); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("CompareAndSwapRef(version skip) error = %v, want %q", err, domain.CodeValidation)
	}

	invalid := remote.Ref{RefName: "other", CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 2}
	if _, err := store.CompareAndSwapRef(ctx, "repo-1", testRefNameForStore, first, invalid); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("CompareAndSwapRef(invalid next) error = %v, want %q", err, domain.CodeValidation)
	}

	second := remote.Ref{RefName: testRefNameForStore, CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 2}
	if _, err := store.CompareAndSwapRef(ctx, "repo-1", testRefNameForStore, first, second); err != nil {
		t.Fatalf("CompareAndSwapRef(second) error = %v", err)
	}
	ref, err = store.ReadRef(ctx, "repo-1", testRefNameForStore)
	if err != nil || ref != second {
		t.Fatalf("ReadRef(after cas) = %+v, %v, want %+v", ref, err, second)
	}
}

func TestGormRefStoreConcurrentCASHasExactlyOneWinner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "refs.db")
	store := openTestRefStore(t, path)
	commitID, _ := testObject("contended ref target")

	const contenders = 8
	results := make(chan error, contenders)
	next := remote.Ref{RefName: testRefNameForStore, CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 1}
	for range contenders {
		go func() {
			_, err := store.CompareAndSwapRef(ctx, "repo-1", testRefNameForStore, remote.Ref{RefName: testRefNameForStore}, next)
			results <- err
		}()
	}
	winners, conflicts := 0, 0
	for range contenders {
		err := <-results
		switch {
		case err == nil:
			winners++
		case domain.CodeOf(err) == domain.CodeRemoteConflict:
			conflicts++
		default:
			t.Fatalf("CompareAndSwapRef(concurrent) unexpected error = %v", err)
		}
	}
	if winners != 1 || conflicts != contenders-1 {
		t.Fatalf("concurrent CAS winners = %d, conflicts = %d, want exactly one winner", winners, conflicts)
	}
	ref, err := store.ReadRef(ctx, "repo-1", testRefNameForStore)
	if err != nil || ref != next {
		t.Fatalf("ReadRef(after contention) = %+v, %v, want %+v", ref, err, next)
	}
}

func TestGormRefStoreIsolatesRepositoriesAndSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "refs.db")
	store := openTestRefStore(t, path)
	commitID, _ := testObject("isolated ref target")

	first := remote.Ref{RefName: testRefNameForStore, CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 1}
	if _, err := store.CompareAndSwapRef(ctx, "repo-1", testRefNameForStore, remote.Ref{RefName: testRefNameForStore}, first); err != nil {
		t.Fatalf("CompareAndSwapRef(repo-1) error = %v", err)
	}

	ref, err := store.ReadRef(ctx, "repo-2", testRefNameForStore)
	if err != nil || ref.Version != 0 {
		t.Fatalf("ReadRef(repo-2) = %+v, %v, want zero-version ref", ref, err)
	}
	if _, err := store.CompareAndSwapRef(ctx, "repo-2", testRefNameForStore, remote.Ref{RefName: testRefNameForStore}, first); err != nil {
		t.Fatalf("CompareAndSwapRef(repo-2) error = %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened := openTestRefStore(t, path)
	ref, err = reopened.ReadRef(ctx, "repo-1", testRefNameForStore)
	if err != nil || ref != first {
		t.Fatalf("ReadRef(after reopen) = %+v, %v, want %+v", ref, err, first)
	}
}
