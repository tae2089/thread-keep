package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/remote"
	"github.com/zeebo/blake3"
)

func gcObject(t *testing.T, parents []string, marker string) (string, []byte) {
	t.Helper()
	payload := map[string]any{"schema_version": 3, "message": marker}
	if len(parents) > 0 {
		payload["parent_ids"] = parents
	}
	contents, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	digest := blake3.Sum256(contents)
	return fmt.Sprintf("%x", digest[:]), contents
}

func publishForGC(t *testing.T, store *CompositeStorage, id string, contents []byte, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.PublishObject(ctx, "repo-1", id, contents); err != nil {
		t.Fatalf("PublishObject(%s) error = %v", id, err)
	}
	if age > 0 {
		path := filepath.Join(store.objects.root, "repo-1", "objects", id+".json")
		past := time.Now().Add(-age)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatalf("Chtimes(%s) error = %v", id, err)
		}
	}
}

func setTip(t *testing.T, store *CompositeStorage, commitID string) {
	t.Helper()
	next := remote.Ref{RefName: "refs/contexts/main", CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 1}
	if _, err := store.CompareAndSwapRef(context.Background(), "repo-1", "refs/contexts/main", remote.Ref{RefName: "refs/contexts/main"}, next); err != nil {
		t.Fatalf("CompareAndSwapRef() error = %v", err)
	}
}

func holdMaintenanceLock(t *testing.T, root string) func() {
	t.Helper()
	lock, acquired, err := acquireMaintenanceLock(root)
	if err != nil {
		t.Fatalf("acquireMaintenanceLock() error = %v", err)
	}
	if !acquired {
		t.Fatal("acquireMaintenanceLock() acquired = false, want true")
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if err := unlockMaintenanceFile(lock); err != nil {
			_ = lock.Close()
			t.Errorf("unlockMaintenanceFile() error = %v", err)
			return
		}
		if err := lock.Close(); err != nil {
			t.Errorf("Close(maintenance lock) error = %v", err)
		}
	}
	t.Cleanup(release)
	return release
}

func gcWasSkipped(t *testing.T, result GCResult) bool {
	t.Helper()
	contents, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal(GCResult) error = %v", err)
	}
	var envelope struct {
		Skipped bool `json:"skipped"`
	}
	if err := json.Unmarshal(contents, &envelope); err != nil {
		t.Fatalf("Unmarshal(GCResult) error = %v", err)
	}
	return envelope.Skipped
}

func TestGormRefStoreListRefsReturnsRepositoryRefs(t *testing.T) {
	ctx := context.Background()
	store := openTestRefStore(t, filepath.Join(t.TempDir(), "refs.db"))
	commitID, _ := testObject("listed ref target")
	first := remote.Ref{RefName: "refs/contexts/main", CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 1}
	if _, err := store.CompareAndSwapRef(ctx, "repo-1", "refs/contexts/main", remote.Ref{RefName: "refs/contexts/main"}, first); err != nil {
		t.Fatalf("CompareAndSwapRef() error = %v", err)
	}
	refs, err := store.ListRefs(ctx, "repo-1")
	if err != nil || len(refs) != 1 || refs[0] != first {
		t.Fatalf("ListRefs(repo-1) = %+v, %v, want the stored ref", refs, err)
	}
	refs, err = store.ListRefs(ctx, "repo-2")
	if err != nil || len(refs) != 0 {
		t.Fatalf("ListRefs(repo-2) = %+v, %v, want empty", refs, err)
	}
}

func TestGCDeletesOnlyAgedUnreachableObjects(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rootID, rootContents := gcObject(t, nil, "root")
	middleID, middleContents := gcObject(t, []string{rootID}, "middle")
	tipID, tipContents := gcObject(t, []string{middleID}, "tip")
	agedOrphanID, agedOrphanContents := gcObject(t, nil, "aged orphan")
	freshOrphanID, freshOrphanContents := gcObject(t, nil, "fresh orphan")

	old := 48 * time.Hour
	publishForGC(t, store, rootID, rootContents, old)
	publishForGC(t, store, middleID, middleContents, old)
	publishForGC(t, store, tipID, tipContents, old)
	publishForGC(t, store, agedOrphanID, agedOrphanContents, old)
	publishForGC(t, store, freshOrphanID, freshOrphanContents, 0)
	setTip(t, store, tipID)

	result, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	repo := result.Repositories["repo-1"]
	if repo.Deleted != 1 || repo.Kept != 4 || repo.Aborted {
		t.Fatalf("RunGC() = %+v, want 1 deleted (aged orphan), 4 kept", repo)
	}
	if _, err := store.ReadObject(ctx, "repo-1", agedOrphanID); !isMissingObjectError(err) {
		t.Fatalf("aged orphan survived GC: %v", err)
	}
	for _, keep := range []string{rootID, middleID, tipID, freshOrphanID} {
		if _, err := store.ReadObject(ctx, "repo-1", keep); err != nil {
			t.Fatalf("reachable/fresh object %s was deleted: %v", keep, err)
		}
	}
}

func TestGCProtectsAgedObjectAfterIdempotentRepublish(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	republishedID, republishedContents := gcObject(t, nil, "republished orphan")
	controlID, controlContents := gcObject(t, nil, "orphan without republish")
	publishForGC(t, store, republishedID, republishedContents, 48*time.Hour)
	publishForGC(t, store, controlID, controlContents, 48*time.Hour)

	created, err := store.PublishObject(ctx, "repo-1", republishedID, republishedContents)
	if err != nil || created {
		t.Fatalf("PublishObject(existing) = %t, %v; want false, nil", created, err)
	}
	republishedPath := filepath.Join(store.objects.root, "repo-1", "objects", republishedID+".json")
	info, err := os.Stat(republishedPath)
	if err != nil {
		t.Fatalf("Stat(republished object) error = %v", err)
	}
	if info.ModTime().Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("republished object mtime = %v; want fresh", info.ModTime())
	}

	result, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	repo := result.Repositories["repo-1"]
	if repo.Deleted != 1 || repo.Kept != 1 || repo.Aborted {
		t.Fatalf("RunGC() = %+v; want republished object kept and control deleted", repo)
	}
	if _, err := store.ReadObject(ctx, "repo-1", republishedID); err != nil {
		t.Fatalf("republished object was deleted: %v", err)
	}
	if _, err := store.ReadObject(ctx, "repo-1", controlID); !isMissingObjectError(err) {
		t.Fatalf("object without republish survived GC: %v", err)
	}
}

func TestGCAbortsRepositoryWhenDAGIsIncomplete(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	missingID, _ := gcObject(t, nil, "never stored")
	tipID, tipContents := gcObject(t, []string{missingID}, "tip with missing parent")
	agedOrphanID, agedOrphanContents := gcObject(t, nil, "would-be victim")
	publishForGC(t, store, tipID, tipContents, 48*time.Hour)
	publishForGC(t, store, agedOrphanID, agedOrphanContents, 48*time.Hour)
	setTip(t, store, tipID)

	result, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	repo := result.Repositories["repo-1"]
	if !repo.Aborted || repo.Deleted != 0 {
		t.Fatalf("RunGC(incomplete DAG) = %+v, want aborted with zero deletions", repo)
	}
	if _, err := store.ReadObject(ctx, "repo-1", agedOrphanID); err != nil {
		t.Fatalf("object deleted despite aborted GC: %v", err)
	}
}

func TestRunGCSkipsWhileMaintenanceLockIsHeldAndRunsAfterRelease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := OpenStorage(root, "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	orphanID, orphanContents := gcObject(t, nil, "concurrent maintenance orphan")
	publishForGC(t, store, orphanID, orphanContents, 48*time.Hour)
	release := holdMaintenanceLock(t, root)

	skipped, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil || !gcWasSkipped(t, skipped) {
		t.Fatalf("RunGC(while locked) = %+v, %v; want observable skipped result", skipped, err)
	}
	release()

	performed, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil || gcWasSkipped(t, performed) || performed.Repositories["repo-1"].Deleted != 1 {
		t.Fatalf("RunGC(after release) = %+v, %v; want one deleted object", performed, err)
	}
}

func TestRunGCStorageLockIsSharedAndReleasedAcrossOpenStorageInstances(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	first, err := OpenStorage(root, "")
	if err != nil {
		t.Fatalf("OpenStorage(first) error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenStorage(root, "")
	if err != nil {
		t.Fatalf("OpenStorage(second) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	orphanID, orphanContents := gcObject(t, nil, "cross-instance maintenance orphan")
	publishForGC(t, first, orphanID, orphanContents, 48*time.Hour)
	release := holdMaintenanceLock(t, first.objects.root)

	skipped, err := RunGC(ctx, second, []string{"repo-1"}, 24*time.Hour)
	if err != nil || !gcWasSkipped(t, skipped) {
		t.Fatalf("RunGC(second storage while locked) = %+v, %v; want skipped", skipped, err)
	}
	release()

	after, err := RunGC(ctx, second, []string{"repo-1"}, 24*time.Hour)
	if err != nil || gcWasSkipped(t, after) || after.Repositories["repo-1"].Deleted != 1 {
		t.Fatalf("RunGC(after lock release) = %+v, %v; want one deleted object", after, err)
	}
}

func TestMaintenancePolicyGitParityDefaults(t *testing.T) {
	policy, err := ResolveMaintenancePolicy(nil)
	if err != nil || !policy.Auto || policy.AutoThreshold != 512 || policy.Grace != 14*24*time.Hour || policy.Interval != 0 {
		t.Fatalf("ResolveMaintenancePolicy(nil) = %+v, %v, want auto on, threshold 512, 2-week grace", policy, err)
	}
	disabled := false
	policy, err = ResolveMaintenancePolicy(&GCConfig{Auto: &disabled, IntervalSeconds: 3600})
	if err != nil || policy.Auto || policy.Interval != 0 {
		t.Fatalf("ResolveMaintenancePolicy(auto off) = %+v, %v, want everything automatic disabled", policy, err)
	}
	policy, err = ResolveMaintenancePolicy(&GCConfig{IntervalSeconds: 3600, GraceSeconds: 3600, AutoThreshold: 10})
	if err != nil || policy.Interval != time.Hour || policy.Grace != time.Hour || policy.AutoThreshold != 10 {
		t.Fatalf("ResolveMaintenancePolicy(explicit) = %+v, %v", policy, err)
	}
	if _, err := ResolveMaintenancePolicy(&GCConfig{GraceSeconds: 30}); err == nil {
		t.Fatalf("ResolveMaintenancePolicy(grace under a minute) expected validation error")
	}
	if _, err := ResolveMaintenancePolicy(&GCConfig{AutoThreshold: -1}); err == nil {
		t.Fatalf("ResolveMaintenancePolicy(negative threshold) expected validation error")
	}
}

func TestMaintainerTriggersOnPublishOverThreshold(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tipID, tipContents := gcObject(t, nil, "auto maintenance tip")
	orphanID, orphanContents := gcObject(t, nil, "auto maintenance orphan")
	publishForGC(t, store, tipID, tipContents, 48*time.Hour)
	publishForGC(t, store, orphanID, orphanContents, 48*time.Hour)
	setTip(t, store, tipID)

	maintainer := NewMaintainer(store, MaintenancePolicy{Auto: true, AutoThreshold: 2, Grace: 24 * time.Hour})
	maintained := maintainer.Wrap(store)
	triggerID, triggerContents := gcObject(t, nil, "publish that crosses threshold")
	if _, err := maintained.PublishObject(ctx, "repo-1", triggerID, triggerContents); err != nil {
		t.Fatalf("PublishObject(trigger) error = %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := store.ReadObject(ctx, "repo-1", orphanID); isMissingObjectError(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("auto maintenance did not collect the aged orphan")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, keep := range []string{tipID, triggerID} {
		if _, err := store.ReadObject(ctx, "repo-1", keep); err != nil {
			t.Fatalf("auto maintenance deleted %s: %v", keep, err)
		}
	}

	off := NewMaintainer(store, MaintenancePolicy{Auto: false, AutoThreshold: 0, Grace: 24 * time.Hour})
	offWrapped := off.Wrap(store)
	quietID, quietContents := gcObject(t, nil, "publish without maintenance")
	if _, err := offWrapped.PublishObject(ctx, "repo-1", quietID, quietContents); err != nil {
		t.Fatalf("PublishObject(auto off) error = %v", err)
	}
}

func TestRunPeriodicGCCollectsAgedOrphans(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tipID, tipContents := gcObject(t, nil, "periodic tip")
	orphanID, orphanContents := gcObject(t, nil, "periodic orphan")
	publishForGC(t, store, tipID, tipContents, 48*time.Hour)
	publishForGC(t, store, orphanID, orphanContents, 48*time.Hour)
	setTip(t, store, tipID)

	go RunPeriodicGC(ctx, store, []string{"repo-1"}, time.Hour, 20*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := store.ReadObject(ctx, "repo-1", orphanID); isMissingObjectError(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic GC did not collect the aged orphan")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := store.ReadObject(ctx, "repo-1", tipID); err != nil {
		t.Fatalf("periodic GC deleted a reachable object: %v", err)
	}
}

func TestGCTreatsUnparseableObjectsAsLeaves(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	opaqueTipID, opaqueTipContents := testObject("not json but referenced by ref")
	opaqueOrphanID, opaqueOrphanContents := testObject("not json and unreferenced")
	publishForGC(t, store, opaqueTipID, opaqueTipContents, 48*time.Hour)
	publishForGC(t, store, opaqueOrphanID, opaqueOrphanContents, 48*time.Hour)
	setTip(t, store, opaqueTipID)

	result, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	repo := result.Repositories["repo-1"]
	if repo.Deleted != 1 || repo.Kept != 1 || repo.Aborted {
		t.Fatalf("RunGC(opaque) = %+v, want orphan deleted and tip kept", repo)
	}
	if _, err := store.ReadObject(ctx, "repo-1", opaqueTipID); err != nil {
		t.Fatalf("ref-target opaque object deleted: %v", err)
	}
}
