package domain

import "testing"

type bindingReconciliationTest struct {
	name          string
	previous      Entity
	current       []Entity
	wantChanged   bool
	wantKey       string
	wantState     NoteBindingState
	wantReason    string
	wantPending   bool
	wantSourceSHA string
}

func TestBindingReconcilerPreservesCurrentBindingRules(t *testing.T) {
	const (
		oldSource = "old-source"
		newSource = "new-source"
	)
	previous := Entity{Language: "go", Key: "example.Run", Kind: EntityFunction, StructuralHash: "stable", SourceSHA: oldSource}
	note := Note{ID: "note", RevisionID: "revision", EntityKey: previous.Key, BindingState: NoteBindingActive, BindingSourceSHA: oldSource}
	tests := []bindingReconciliationTest{
		{
			name:          "unknown lineage",
			current:       []Entity{{Language: "go", Key: previous.Key, Kind: EntityFunction, StructuralHash: "stable", SourceSHA: newSource}},
			wantChanged:   true,
			wantKey:       previous.Key,
			wantState:     NoteBindingNeedsReview,
			wantReason:    "unknown_lineage",
			wantPending:   true,
			wantSourceSHA: newSource,
		},
		{
			name:          "exact structural match",
			previous:      previous,
			current:       []Entity{{Language: "go", Key: previous.Key, Kind: EntityFunction, StructuralHash: "stable", SourceSHA: newSource}},
			wantChanged:   true,
			wantKey:       previous.Key,
			wantState:     NoteBindingActive,
			wantPending:   true,
			wantSourceSHA: newSource,
		},
		{
			name:          "exact key structural change",
			previous:      previous,
			current:       []Entity{{Language: "go", Key: previous.Key, Kind: EntityFunction, StructuralHash: "changed", SourceSHA: newSource}},
			wantChanged:   true,
			wantKey:       previous.Key,
			wantState:     NoteBindingNeedsReview,
			wantReason:    "structural_change",
			wantPending:   true,
			wantSourceSHA: newSource,
		},
		{
			name:          "unique structural move",
			previous:      previous,
			current:       []Entity{{Language: "go", Key: "moved.Run", Kind: EntityFunction, StructuralHash: "stable", SourceSHA: newSource}},
			wantChanged:   true,
			wantKey:       "moved.Run",
			wantState:     NoteBindingActive,
			wantPending:   true,
			wantSourceSHA: newSource,
		},
		{
			name:     "ambiguous structural move",
			previous: previous,
			current: []Entity{
				{Language: "go", Key: "first.Run", Kind: EntityFunction, StructuralHash: "stable", SourceSHA: newSource},
				{Language: "go", Key: "second.Run", Kind: EntityFunction, StructuralHash: "stable", SourceSHA: newSource},
			},
			wantChanged:   true,
			wantKey:       previous.Key,
			wantState:     NoteBindingNeedsReview,
			wantReason:    "ambiguous_lineage",
			wantPending:   true,
			wantSourceSHA: newSource,
		},
		{
			name:          "removed entity",
			previous:      previous,
			wantChanged:   true,
			wantKey:       previous.Key,
			wantState:     NoteBindingHistorical,
			wantReason:    "entity_removed",
			wantPending:   true,
			wantSourceSHA: newSource,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reconciler := NewBindingReconciler(test.current, newSource)
			got, changed := reconciler.Reconcile(note, test.previous)
			if changed != test.wantChanged || got.EntityKey != test.wantKey || got.BindingState != test.wantState || got.ReviewReason != test.wantReason || got.Pending != test.wantPending || got.BindingSourceSHA != test.wantSourceSHA {
				t.Fatalf("Reconcile() = (%+v, %t), want changed=%t key=%q state=%q reason=%q pending=%t source=%q", got, changed, test.wantChanged, test.wantKey, test.wantState, test.wantReason, test.wantPending, test.wantSourceSHA)
			}
		})
	}
}

func TestBindingReconcilerReportsNoChangeForCurrentExactBinding(t *testing.T) {
	const sourceSHA = "source"
	entity := Entity{Language: "go", Key: "example.Run", Kind: EntityFunction, StructuralHash: "stable", SourceSHA: sourceSHA}
	note := Note{ID: "note", RevisionID: "revision", EntityKey: entity.Key, BindingState: NoteBindingActive, BindingSourceSHA: sourceSHA}

	got, changed := NewBindingReconciler([]Entity{entity}, sourceSHA).Reconcile(note, entity)
	if changed {
		t.Fatalf("Reconcile() changed = true, want false: %+v", got)
	}
}

func TestPlanSnapshotMergeAcceptsV3AndV4SnapshotFamily(t *testing.T) {
	base := testMergeSnapshot([]Note{testMergeNote("shared", "base-revision")})
	local := testMergeSnapshot([]Note{testMergeNote("shared", "base-revision"), testMergeNote("local", "local-revision")})
	remote := testMergeSnapshot([]Note{testMergeNote("shared", "base-revision"), testMergeNote("remote", "remote-revision")})
	remote.SchemaVersion = 4

	if _, err := PlanSnapshotMerge(SnapshotMergeInput{Base: base, Local: local, Remote: remote}); err != nil {
		t.Fatalf("PlanSnapshotMerge(v3/v4) error = %v", err)
	}
}
