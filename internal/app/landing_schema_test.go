package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/store"
	"github.com/zeebo/blake3"
)

func TestCommitPreservesSchemaV4AfterLandingSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "base note", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	baseCommit, err := svc.Commit(ctx, CommitInput{Message: "base", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(base) error = %v", err)
	}
	_, key, err := svc.mutableKey(ctx)
	if err != nil {
		t.Fatalf("mutableKey() error = %v", err)
	}
	contextStore, err := svc.openStore(ctx, false)
	if err != nil {
		t.Fatalf("openStore() error = %v", err)
	}
	base, err := contextStore.ReadContextObject(baseCommit.ID, key.RepositoryID, key.RefName)
	if err != nil {
		t.Fatalf("ReadContextObject(base) error = %v", err)
	}
	landing := base
	landing.SchemaVersion = 4
	landing.ParentIDs = []string{baseCommit.ID}
	landing.LegacyParentID = ""
	landing.Message = "landing"
	landing.CreatedAt = base.CreatedAt.Add(time.Second)
	landing.LandingReceipts = []domain.LandingReceipt{appTestLandingReceipt(landing, baseCommit.ID)}
	landingID := writeAppTestContextObject(t, contextStore, landing)
	currentRef, err := contextStore.ContextRef(ctx, key)
	if err != nil {
		t.Fatalf("ContextRef() error = %v", err)
	}
	nextRef := domain.ContextRef{RefName: key.RefName, CommitID: landingID, SourceSHA: key.SourceSHA, Version: currentRef.Version + 1}
	if err := contextStore.FastForward(ctx, store.FastForwardInput{Key: key, Expected: currentRef, Next: nextRef}); err != nil {
		t.Fatalf("FastForward(v4) error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "warning", Body: "follow-up", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(follow-up) error = %v", err)
	}
	commit, err := svc.Commit(ctx, CommitInput{Message: "after landing", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(after v4) error = %v", err)
	}
	object, err := contextStore.ReadContextObject(commit.ID, key.RepositoryID, key.RefName)
	if err != nil {
		t.Fatalf("ReadContextObject(after v4) error = %v", err)
	}
	if object.SchemaVersion != 4 || len(object.LandingReceipts) != 0 || len(object.ParentIDs) != 1 || object.ParentIDs[0] != landingID {
		t.Fatalf("committed object = %+v, want receipt-free v4 child of landing", object)
	}
}

func TestContextSnapshotWriteVersionUsesV4WhenAnyParentIsV4(t *testing.T) {
	if got := contextSnapshotWriteVersion(domain.ContextObject{SchemaVersion: 3}); got != 3 {
		t.Fatalf("contextSnapshotWriteVersion(v3) = %d, want 3", got)
	}
	if got := contextSnapshotWriteVersion(domain.ContextObject{SchemaVersion: 3}, domain.ContextObject{SchemaVersion: 4}); got != 4 {
		t.Fatalf("contextSnapshotWriteVersion(v3, v4) = %d, want 4", got)
	}
}

func appTestLandingReceipt(object domain.ContextObject, baseContextCommitID string) domain.LandingReceipt {
	return domain.LandingReceipt{
		ID:                  strings.Repeat("a", 64),
		Provider:            "github",
		ForgeRepository:     "owner/repository",
		ChangeNumber:        42,
		ContextRepositoryID: object.RepositoryID,
		TargetRef:           object.RefName,
		CandidateDigest:     strings.Repeat("b", 64),
		FinalPlanID:         strings.Repeat("c", 64),
		SourceMergeSHA:      object.SourceSHA,
		BaseContextCommitID: baseContextCommitID,
		Resolver:            "automatic",
	}
}

func writeAppTestContextObject(t *testing.T, contextStore *store.Store, object domain.ContextObject) string {
	t.Helper()
	contents, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	digest := blake3.Sum256(contents)
	identifier := fmt.Sprintf("%x", digest[:])
	if err := contextStore.WriteObject(identifier, contents); err != nil {
		t.Fatalf("WriteObject() error = %v", err)
	}
	return identifier
}
