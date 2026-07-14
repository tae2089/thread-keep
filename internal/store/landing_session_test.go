package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestLandingSessionPersistsAndUsesVersionCAS(t *testing.T) {
	contextStore, err := Open(context.Background(), NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })
	now := time.Now().UTC()
	session := domain.LandingSession{LandingID: strings.Repeat("a", 64), RemoteName: "origin", RepositoryID: "repo", RefName: "refs/contexts/main", SourceSHA: strings.Repeat("1", 40), ExpectedRemoteCommitID: strings.Repeat("b", 64), ExpectedRemoteRefVersion: 1, Plan: domain.ContextPlan{Kind: domain.ContextPlanFinal, Fingerprint: domain.PlanFingerprint{HeadSourceSHA: strings.Repeat("1", 40)}, Outcome: domain.ContextPlanBlocked, Conflicts: []domain.LandingConflict{{NoteID: "note", Reason: "revision_conflict"}}}, Entities: []domain.Entity{{Key: "example.Value"}}, Provenance: []domain.ContextSnapshotProvenance{{Language: "go"}}, CreatedAt: now}
	created, err := contextStore.CreateLandingSession(context.Background(), session)
	if err != nil || created.ID == "" || created.Version != 1 || created.State != domain.LandingSessionOpen {
		t.Fatalf("CreateLandingSession() = %+v, %v", created, err)
	}
	created.Plan.Conflicts = nil
	updated, err := contextStore.UpdateLandingSession(context.Background(), created.Version, created)
	if err != nil || updated.Version != 2 || updated.State != domain.LandingSessionReady {
		t.Fatalf("UpdateLandingSession() = %+v, %v", updated, err)
	}
	if _, err := contextStore.UpdateLandingSession(context.Background(), 1, created); domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("UpdateLandingSession(stale) error = %v", err)
	}
	loaded, err := contextStore.LandingSession(context.Background(), created.ID)
	if err != nil || loaded.Version != 2 || loaded.State != domain.LandingSessionReady {
		t.Fatalf("LandingSession() = %+v, %v", loaded, err)
	}
}

func TestPrepareLandingRecoveryFastForwardsContextAcrossSourceGap(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })
	baseSource := strings.Repeat("1", 40)
	mergeSource := strings.Repeat("2", 40)
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: baseSource}
	base := testV3SnapshotObject(baseSource, nil, nil)
	base.RepositoryID = key.RepositoryID
	base.RefName = key.RefName
	baseID := writeTestContextObject(t, contextStore, base)
	if _, _, err := contextStore.Rebuild(ctx, RebuildInput{Key: key, CommitID: baseID, Projections: testProjection(key)}); err != nil {
		t.Fatalf("Rebuild(base) error = %v", err)
	}
	canonical := testV3SnapshotObject(baseSource, []string{baseID}, nil)
	canonical.RepositoryID = key.RepositoryID
	canonical.RefName = key.RefName
	canonicalID := writeTestContextObject(t, contextStore, canonical)
	mergeKey := key
	mergeKey.SourceSHA = mergeSource
	if err := contextStore.ApplyIndexUpdate(ctx, mergeKey, testProjection(mergeKey)); err != nil {
		t.Fatalf("ApplyIndexUpdate(merge) error = %v", err)
	}
	expected, err := contextStore.ContextRef(ctx, mergeKey)
	if err != nil {
		t.Fatalf("ContextRef() error = %v", err)
	}
	next := domain.ContextRef{RefName: key.RefName, CommitID: canonicalID, SourceSHA: baseSource, Version: 2}
	if err := contextStore.PrepareLandingRecovery(ctx, PrepareLandingRecoveryInput{Key: mergeKey, Expected: expected, Next: next}); err != nil {
		t.Fatalf("PrepareLandingRecovery() error = %v", err)
	}
	ref, err := contextStore.ContextRef(ctx, mergeKey)
	if err != nil || ref.CommitID != canonicalID || ref.SourceSHA != baseSource || ref.Version != expected.Version+1 {
		t.Fatalf("ContextRef(after recovery prepare) = %+v, %v", ref, err)
	}
	snapshot, err := contextStore.CommitSnapshot(ctx, mergeKey)
	if err != nil || snapshot.ParentID != canonicalID || snapshot.WorkingSource != mergeSource {
		t.Fatalf("CommitSnapshot(after recovery prepare) = %+v, %v", snapshot, err)
	}
}
