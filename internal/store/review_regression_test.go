package store

import (
	"context"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestFinalizeCommitRejectsChangedPendingNoteSnapshot(t *testing.T) {
	mutations := map[string]func(t *testing.T, contextStore *Store, key domain.WorkingSetKey, note domain.Note){
		"revision": func(t *testing.T, contextStore *Store, key domain.WorkingSetKey, note domain.Note) {
			t.Helper()
			note.RevisionID = "replacement-revision"
			note.Body = "replacement body"
			if _, err := contextStore.AddPendingNote(context.Background(), key, note); err != nil {
				t.Fatalf("AddPendingNote(replacement) error = %v", err)
			}
		},
		"binding": func(t *testing.T, contextStore *Store, key domain.WorkingSetKey, note domain.Note) {
			t.Helper()
			if _, err := contextStore.ReviewNote(context.Background(), key, note.ID, note.EntityKey); err != nil {
				t.Fatalf("ReviewNote() error = %v", err)
			}
		},
		"content": func(t *testing.T, contextStore *Store, key domain.WorkingSetKey, note domain.Note) {
			t.Helper()
			note.Body = "silently replaced body"
			if _, err := contextStore.AddPendingNote(context.Background(), key, note); err != nil {
				t.Fatalf("AddPendingNote(changed content) error = %v", err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			contextStore, key, entity := newReviewRegressionStore(t, ctx)
			note, err := contextStore.AddPendingNote(ctx, key, domain.Note{
				ID:               "note",
				RevisionID:       "snapshot-revision",
				EntityKey:        entity.Key,
				Kind:             domain.NoteIntent,
				Body:             "snapshot body",
				Author:           "tester",
				Origin:           "human",
				BindingState:     domain.NoteBindingNeedsReview,
				BindingSourceSHA: key.SourceSHA,
				ReviewReason:     "structural_change",
				Topics:           []string{"contract"},
			})
			if err != nil {
				t.Fatalf("AddPendingNote() error = %v", err)
			}
			snapshot, err := contextStore.CommitSnapshot(ctx, key)
			if err != nil {
				t.Fatalf("CommitSnapshot() error = %v", err)
			}
			mutate(t, contextStore, key, note)
			snapshotNote := snapshot.PendingNotes[0]
			snapshotNote.Pending = false

			err = contextStore.FinalizeCommit(ctx, FinalizeInput{
				Key:            key,
				ExpectedParent: snapshot.ParentID,
				PendingNoteIDs: []string{note.ID},
				Commit: domain.ContextCommit{
					ID:        "stale-snapshot-commit",
					RefName:   key.RefName,
					SourceSHA: key.SourceSHA,
					Message:   "stale snapshot",
					Author:    "tester",
					CreatedAt: time.Now().UTC(),
				},
				Notes: []domain.Note{snapshotNote},
			})
			if domain.CodeOf(err) != domain.CodeConcurrentUpdate {
				t.Fatalf("FinalizeCommit() error = %v, want concurrent update", err)
			}
			pending, pendingErr := contextStore.PendingNotes(ctx, key)
			if pendingErr != nil || len(pending) != 1 {
				t.Fatalf("PendingNotes() = %+v, %v; want changed note preserved", pending, pendingErr)
			}
		})
	}
}

func TestContextRejectsSameSourceFromDifferentWorkingSet(t *testing.T) {
	ctx := context.Background()
	contextStore, key, entity := newReviewRegressionStore(t, ctx)
	otherRef := key
	otherRef.RefName = "refs/contexts/other"

	if _, _, err := contextStore.Context(ctx, otherRef, entity.Key); domain.CodeOf(err) != domain.CodeStaleWorkingSet {
		t.Fatalf("Context(other ref) error = %v, want stale working set", err)
	}
}

func TestAssembleContextUsesRequestedNoteStatesForRoots(t *testing.T) {
	ctx := context.Background()
	contextStore, key, entity := newReviewRegressionStore(t, ctx)
	note, err := contextStore.AddPendingNote(ctx, key, domain.Note{
		ID:               "needs-review-note",
		RevisionID:       "needs-review-revision",
		EntityKey:        entity.Key,
		Kind:             domain.NoteWarning,
		Body:             "migration invariant",
		Author:           "tester",
		Origin:           "human",
		BindingState:     domain.NoteBindingNeedsReview,
		BindingSourceSHA: key.SourceSHA,
		ReviewReason:     "structural_change",
		Topics:           []string{"migration"},
	})
	if err != nil {
		t.Fatalf("AddPendingNote() error = %v", err)
	}
	tests := map[string]domain.ContextQuery{
		"text": {
			Anchor: domain.ContextAnchor{Kind: domain.AnchorText, Query: "migration invariant"},
			States: []domain.NoteBindingState{domain.NoteBindingNeedsReview},
		},
		"topic": {
			Anchor: domain.ContextAnchor{Kind: domain.AnchorText, Query: "unmatched-anchor"},
			States: []domain.NoteBindingState{domain.NoteBindingNeedsReview},
			Topics: []string{"migration"},
		},
	}
	for name, query := range tests {
		t.Run(name, func(t *testing.T) {
			bundle, err := contextStore.AssembleContext(ctx, key, query)
			if err != nil {
				t.Fatalf("AssembleContext() error = %v", err)
			}
			if len(bundle.Items) != 1 || bundle.Items[0].Note.ID != note.ID || bundle.Items[0].Note.BindingState != domain.NoteBindingNeedsReview {
				t.Fatalf("AssembleContext() items = %+v, want needs-review note", bundle.Items)
			}
		})
	}
}

func TestAssembleContextUsesPreviousEntityForReconciledStateRoots(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })

	oldKey := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: "old-source"}
	oldEntity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "run.go", SourceSHA: oldKey.SourceSHA, StructuralHash: "shared-hash"}
	note := domain.Note{ID: "note", RevisionID: "revision", EntityKey: oldEntity.Key, Kind: domain.NoteConstraint, Body: "binding contract", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: oldKey.SourceSHA, Topics: []string{"binding"}}
	object := domain.ContextObject{
		SchemaVersion: 3, RepositoryID: oldKey.RepositoryID, RefName: oldKey.RefName, SourceSHA: oldKey.SourceSHA,
		Message: "base", Author: "tester", CreatedAt: time.Now().UTC(),
		Provenance:       []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: oldKey.SourceSHA}},
		Entities:         []domain.Entity{oldEntity},
		Notes:            []domain.Note{note},
		RevisionMappings: []domain.ContextRevisionMapping{{EntityKey: note.EntityKey, NoteID: note.ID, RevisionID: note.RevisionID, BindingState: note.BindingState, BindingSourceSHA: note.BindingSourceSHA}},
	}
	commitID := writeTestContextObject(t, contextStore, object)
	oldProjection := domain.LanguageProjection{Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: oldKey.SourceSHA}, Entities: []domain.Entity{oldEntity}}
	if _, _, err := contextStore.Rebuild(ctx, RebuildInput{Key: oldKey, CommitID: commitID, Projections: []domain.LanguageProjection{oldProjection}}); err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}

	newKey := oldKey
	newKey.SourceSHA = "new-source"
	projection := domain.LanguageProjection{Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: newKey.SourceSHA}, Entities: []domain.Entity{
		{Language: "go", Key: "example.FirstRun", Kind: domain.EntityFunction, Name: "FirstRun", Path: "first.go", SourceSHA: newKey.SourceSHA, StructuralHash: "shared-hash"},
		{Language: "go", Key: "example.SecondRun", Kind: domain.EntityFunction, Name: "SecondRun", Path: "second.go", SourceSHA: newKey.SourceSHA, StructuralHash: "shared-hash"},
	}}
	if err := contextStore.ApplyIndexUpdate(ctx, newKey, []domain.LanguageProjection{projection}); err != nil {
		t.Fatalf("ApplyIndexUpdate() error = %v", err)
	}
	pending, err := contextStore.PendingNotes(ctx, newKey)
	if err != nil || len(pending) != 1 || pending[0].BindingState != domain.NoteBindingNeedsReview {
		t.Fatalf("PendingNotes() = %+v, %v; want needs_review", pending, err)
	}

	queries := map[string]domain.ContextQuery{
		"text":  {Anchor: domain.ContextAnchor{Kind: domain.AnchorText, Query: "binding contract"}, States: []domain.NoteBindingState{domain.NoteBindingNeedsReview}},
		"topic": {Anchor: domain.ContextAnchor{Kind: domain.AnchorText, Query: "unmatched-anchor"}, States: []domain.NoteBindingState{domain.NoteBindingNeedsReview}, Topics: []string{"binding"}},
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			bundle, err := contextStore.AssembleContext(ctx, newKey, query)
			if err != nil {
				t.Fatalf("AssembleContext() error = %v", err)
			}
			if len(bundle.Items) != 1 || bundle.Items[0].Note.ID != note.ID || bundle.Items[0].BoundEntity.EntityKey != oldEntity.Key {
				t.Fatalf("AssembleContext() items = %+v, want needs-review note with previous entity", bundle.Items)
			}
		})
	}
}

func newReviewRegressionStore(t *testing.T, ctx context.Context) (*Store, domain.WorkingSetKey, domain.Entity) {
	t.Helper()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })
	if err := contextStore.Initialize(ctx); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: "source"}
	entity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: key.SourceSHA, StructuralHash: "hash"}
	projection := domain.LanguageProjection{
		Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA},
		Entities: []domain.Entity{entity},
	}
	if err := contextStore.ApplyIndexUpdate(ctx, key, []domain.LanguageProjection{projection}); err != nil {
		t.Fatalf("ApplyIndexUpdate() error = %v", err)
	}
	return contextStore, key, entity
}
