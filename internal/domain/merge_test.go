package domain

import "testing"

func TestPlanSnapshotMergeComposesNonOverlappingRecords(t *testing.T) {
	base := testMergeSnapshot([]Note{testMergeNote("shared", "base-revision")})
	local := testMergeSnapshot([]Note{testMergeNote("shared", "base-revision"), testMergeNote("local", "local-revision")})
	remote := testMergeSnapshot([]Note{testMergeNote("shared", "base-revision"), testMergeNote("remote", "remote-revision")})

	plan, err := PlanSnapshotMerge(SnapshotMergeInput{Base: base, Local: local, Remote: remote})
	if err != nil {
		t.Fatalf("PlanSnapshotMerge() error = %v", err)
	}
	if len(plan.Records) != 3 || len(plan.Conflicts) != 0 {
		t.Fatalf("PlanSnapshotMerge() = %+v", plan)
	}
	for index, want := range []string{"local", "remote", "shared"} {
		if plan.Records[index].Note.ID != want {
			t.Fatalf("plan records = %+v, want sorted note ID %q at %d", plan.Records, want, index)
		}
	}
}

func TestPlanSnapshotMergeSurfacesCompetingRevisionConflict(t *testing.T) {
	base := testMergeSnapshot([]Note{testMergeNote("shared", "base-revision")})
	local := testMergeSnapshot([]Note{testMergeNote("shared", "local-revision")})
	remote := testMergeSnapshot([]Note{testMergeNote("shared", "remote-revision")})

	plan, err := PlanSnapshotMerge(SnapshotMergeInput{Base: base, Local: local, Remote: remote})
	if err != nil {
		t.Fatalf("PlanSnapshotMerge() error = %v", err)
	}
	if len(plan.Records) != 0 || len(plan.Conflicts) != 1 {
		t.Fatalf("PlanSnapshotMerge() = %+v", plan)
	}
	conflict := plan.Conflicts[0]
	if conflict.NoteID != "shared" || conflict.Base == nil || conflict.Local == nil || conflict.Remote == nil || conflict.Base.Note.RevisionID != "base-revision" || conflict.Local.Note.RevisionID != "local-revision" || conflict.Remote.Note.RevisionID != "remote-revision" {
		t.Fatalf("merge conflict = %+v", conflict)
	}
}

func TestPlanSnapshotMergeRejectsIncompatibleSnapshots(t *testing.T) {
	base := testMergeSnapshot([]Note{testMergeNote("shared", "base-revision")})
	local := testMergeSnapshot([]Note{testMergeNote("shared", "local-revision")})
	for name, remote := range map[string]ContextObject{
		"non-v3": func() ContextObject {
			invalid := testMergeSnapshot([]Note{testMergeNote("shared", "remote-revision")})
			invalid.SchemaVersion = 2
			return invalid
		}(),
		"different-source": func() ContextObject {
			invalid := testMergeSnapshot([]Note{testMergeNote("shared", "remote-revision")})
			invalid.SourceSHA = "other-source"
			return invalid
		}(),
		"different-repository": func() ContextObject {
			invalid := testMergeSnapshot([]Note{testMergeNote("shared", "remote-revision")})
			invalid.RepositoryID = "other-repository"
			return invalid
		}(),
		"different-ref": func() ContextObject {
			invalid := testMergeSnapshot([]Note{testMergeNote("shared", "remote-revision")})
			invalid.RefName = "refs/contexts/other"
			return invalid
		}(),
		"different-provenance": func() ContextObject {
			invalid := testMergeSnapshot([]Note{testMergeNote("shared", "remote-revision")})
			invalid.Provenance = []ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: invalid.SourceSHA}}
			return invalid
		}(),
		"mismatched-mapping": func() ContextObject {
			invalid := testMergeSnapshot([]Note{testMergeNote("shared", "remote-revision")})
			invalid.RevisionMappings[0].RevisionID = "different-revision"
			return invalid
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PlanSnapshotMerge(SnapshotMergeInput{Base: base, Local: local, Remote: remote}); CodeOf(err) != CodeValidation {
				t.Fatalf("PlanSnapshotMerge() error = %v, want validation", err)
			}
		})
	}
}

func testMergeSnapshot(notes []Note) ContextObject {
	mappings := make([]ContextRevisionMapping, 0, len(notes))
	for _, note := range notes {
		mappings = append(mappings, ContextRevisionMapping{EntityKey: note.EntityKey, NoteID: note.ID, RevisionID: note.RevisionID, BindingState: note.BindingState, BindingSourceSHA: note.BindingSourceSHA, ReviewReason: note.ReviewReason})
	}
	return ContextObject{SchemaVersion: 3, RepositoryID: "repo", RefName: "refs/contexts/main", SourceSHA: "source", Notes: notes, RevisionMappings: mappings}
}

func testMergeNote(id, revisionID string) Note {
	return Note{ID: id, RevisionID: revisionID, EntityKey: "example.Run", Kind: NoteIntent, Body: revisionID, Author: "tester", Origin: "human", BindingState: NoteBindingActive, BindingSourceSHA: "source"}
}
