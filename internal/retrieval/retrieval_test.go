package retrieval

import (
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestAssembleValidatesDefaultsAndOrdersEvidence(t *testing.T) {
	t.Parallel()

	if _, err := Assemble(Input{}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Assemble(empty) error = %v, want validation", err)
	}

	root := domain.Entity{Language: "go", Key: "payment.Authorize", Kind: domain.EntityFunction, Name: "Authorize", Path: "payment.go", SourceSHA: "source"}
	direct := domain.Note{ID: "direct", RevisionID: "direct-r1", EntityKey: root.Key, Kind: domain.NoteDecision, Body: "authorize directly", BindingState: domain.NoteBindingActive}

	bundle, err := Assemble(Input{
		Query:  domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: root.Key}},
		Source: domain.RetrievalSource{SourceSHA: "source"},
		Anchor: domain.ResolvedAnchor{Kind: domain.AnchorEntity, EntityKeys: []string{root.Key}},
		Candidates: []Candidate{
			{Root: root, Bound: root, Note: direct, Reasons: []domain.SelectionReason{{Kind: domain.ReasonDirectEntity}}},
			{Root: root, Bound: root, Note: direct, Reasons: []domain.SelectionReason{{Kind: domain.ReasonDirectEntity}}},
		},
		Complete: true,
	})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	if bundle.Source.SourceSHA != "source" || bundle.Anchor.Kind != domain.AnchorEntity || !bundle.Complete {
		t.Fatalf("Assemble() metadata = %+v", bundle)
	}
	if len(bundle.Items) != 1 {
		t.Fatalf("Assemble() items = %+v, want one deduplicated direct note", bundle.Items)
	}
	if bundle.Items[0].Note.ID != direct.ID || bundle.Items[0].Reasons[0].Kind != domain.ReasonDirectEntity {
		t.Fatalf("first item = %+v, want direct evidence first", bundle.Items[0])
	}
}

func TestAssembleDefaultsToActiveCurrentNotesAndAppliesFilters(t *testing.T) {
	t.Parallel()

	entity := domain.Entity{Language: "go", Key: "cache.Invalidate", Kind: domain.EntityFunction, Name: "Invalidate", Path: "cache.go", SourceSHA: "source"}
	candidates := []Candidate{
		{Root: entity, Bound: entity, Note: domain.Note{ID: "active-warning", RevisionID: "r1", EntityKey: entity.Key, Kind: domain.NoteWarning, BindingState: domain.NoteBindingActive}, Reasons: []domain.SelectionReason{{Kind: domain.ReasonDirectEntity}}},
		{Root: entity, Bound: entity, Note: domain.Note{ID: "review-warning", RevisionID: "r2", EntityKey: entity.Key, Kind: domain.NoteWarning, BindingState: domain.NoteBindingNeedsReview}, Reasons: []domain.SelectionReason{{Kind: domain.ReasonDirectEntity}}},
		{Root: entity, Bound: entity, Note: domain.Note{ID: "active-decision", RevisionID: "r3", EntityKey: entity.Key, Kind: domain.NoteDecision, BindingState: domain.NoteBindingActive}, Reasons: []domain.SelectionReason{{Kind: domain.ReasonDirectEntity}}},
	}
	bundle, err := Assemble(Input{
		Query:      domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: entity.Key}, Kinds: []domain.NoteKind{domain.NoteWarning}},
		Source:     domain.RetrievalSource{SourceSHA: "source"},
		Anchor:     domain.ResolvedAnchor{Kind: domain.AnchorEntity, EntityKeys: []string{entity.Key}},
		Candidates: candidates,
		Complete:   true,
	})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	if len(bundle.Items) != 1 || bundle.Items[0].Note.ID != "active-warning" {
		t.Fatalf("Assemble() items = %+v, want only active warning", bundle.Items)
	}
}

func TestAssembleMarksTruncatedBundleIncomplete(t *testing.T) {
	t.Parallel()
	entity := domain.Entity{Language: "go", Key: "cache.Invalidate", Kind: domain.EntityFunction, Name: "Invalidate", Path: "cache.go", SourceSHA: "source"}
	candidates := []Candidate{
		{Root: entity, Bound: entity, Note: domain.Note{ID: "one", RevisionID: "r1", EntityKey: entity.Key, Kind: domain.NoteWarning, BindingState: domain.NoteBindingActive}, Reasons: []domain.SelectionReason{{Kind: domain.ReasonDirectEntity}}},
		{Root: entity, Bound: entity, Note: domain.Note{ID: "two", RevisionID: "r2", EntityKey: entity.Key, Kind: domain.NoteConstraint, BindingState: domain.NoteBindingActive}, Reasons: []domain.SelectionReason{{Kind: domain.ReasonDirectEntity}}},
	}

	bundle, err := Assemble(Input{Query: domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: entity.Key}, Limit: 1}, Source: domain.RetrievalSource{SourceSHA: "source"}, Anchor: domain.ResolvedAnchor{Kind: domain.AnchorEntity, EntityKeys: []string{entity.Key}}, Candidates: candidates, Complete: true})
	if err != nil {
		t.Fatalf("Assemble() error = %v", err)
	}
	if bundle.Complete || len(bundle.Items) != 1 || len(bundle.Diagnostics) != 1 || bundle.Diagnostics[0].Code != "results_truncated" {
		t.Fatalf("bundle = %+v, want one item and explicit incomplete truncation", bundle)
	}
}

func TestClassifyChangesCoversAddedModifiedMovedAndRemoved(t *testing.T) {
	t.Parallel()

	base := []domain.Entity{
		{Language: "go", Key: "example.Modified", Kind: domain.EntityFunction, StructuralHash: "old"},
		{Language: "go", Key: "example.OldName", Kind: domain.EntityFunction, StructuralHash: "moved"},
		{Language: "go", Key: "example.Removed", Kind: domain.EntityFunction, StructuralHash: "removed"},
		{Language: "go", Key: "example.Stable", Kind: domain.EntityFunction, StructuralHash: "stable"},
	}
	current := []domain.Entity{
		{Language: "go", Key: "example.Added", Kind: domain.EntityFunction, StructuralHash: "added"},
		{Language: "go", Key: "example.Modified", Kind: domain.EntityFunction, StructuralHash: "new"},
		{Language: "go", Key: "example.NewName", Kind: domain.EntityFunction, StructuralHash: "moved"},
		{Language: "go", Key: "example.Stable", Kind: domain.EntityFunction, StructuralHash: "stable"},
	}
	changes := ClassifyChanges(base, current)
	if len(changes) != 4 {
		t.Fatalf("ClassifyChanges() = %+v, want four changes", changes)
	}
	got := make(map[domain.EntityChangeKind]domain.EntityChange, len(changes))
	for _, change := range changes {
		got[change.Kind] = change
	}
	if got[domain.ChangeAdded].Target.Key != "example.Added" {
		t.Fatalf("added = %+v", got[domain.ChangeAdded])
	}
	if got[domain.ChangeModified].Base.StructuralHash != "old" || got[domain.ChangeModified].Target.StructuralHash != "new" {
		t.Fatalf("modified = %+v", got[domain.ChangeModified])
	}
	if got[domain.ChangeMoved].Base.Key != "example.OldName" || got[domain.ChangeMoved].Target.Key != "example.NewName" {
		t.Fatalf("moved = %+v", got[domain.ChangeMoved])
	}
	if got[domain.ChangeRemoved].Base.Key != "example.Removed" {
		t.Fatalf("removed = %+v", got[domain.ChangeRemoved])
	}
}

func TestClassifyChangesDoesNotGuessAmbiguousMoves(t *testing.T) {
	t.Parallel()

	base := []domain.Entity{
		{Language: "go", Key: "example.OldA", Kind: domain.EntityFunction, StructuralHash: "same"},
		{Language: "go", Key: "example.OldB", Kind: domain.EntityFunction, StructuralHash: "same"},
	}
	current := []domain.Entity{{Language: "go", Key: "example.New", Kind: domain.EntityFunction, StructuralHash: "same"}}
	changes := ClassifyChanges(base, current)
	var added, removed int
	for _, change := range changes {
		switch change.Kind {
		case domain.ChangeAdded:
			added++
		case domain.ChangeRemoved:
			removed++
		case domain.ChangeMoved:
			t.Fatalf("ClassifyChanges() guessed ambiguous move: %+v", changes)
		}
	}
	if added != 1 || removed != 2 {
		t.Fatalf("ClassifyChanges() = %+v, want one added and two removed", changes)
	}
}
