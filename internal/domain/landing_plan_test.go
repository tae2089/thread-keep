package domain

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

type landingPlanFixture struct {
	input LandingPlanInput
	note  Note
}

func TestPlanLandingClassifiesReadyReviewAndBlocked(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		fixture := newLandingPlanFixture(t)
		plan, err := PlanLanding(fixture.input)
		if err != nil {
			t.Fatalf("PlanLanding() error = %v", err)
		}
		if plan.Outcome != ContextPlanReady || len(plan.Decisions) != 1 || len(plan.Conflicts) != 0 || plan.Decisions[0].Note.BindingState != NoteBindingActive {
			t.Fatalf("PlanLanding() = %+v, want ready active decision", plan)
		}
	})

	t.Run("review required", func(t *testing.T) {
		fixture := newLandingPlanFixture(t)
		fixture.input.TargetEntities[0].StructuralHash = strings.Repeat("9", 64)
		fixture.input.Fingerprint.SourceEvidenceDigest = DigestSourceEvidence(fixture.input.TargetEntities)
		plan, err := PlanLanding(fixture.input)
		if err != nil {
			t.Fatalf("PlanLanding() error = %v", err)
		}
		if plan.Outcome != ContextPlanReviewRequired || plan.Decisions[0].Note.BindingState != NoteBindingNeedsReview || plan.Decisions[0].Note.ReviewReason != "structural_change" {
			t.Fatalf("PlanLanding() = %+v, want review_required structural change", plan)
		}
	})

	t.Run("blocked incomplete coverage", func(t *testing.T) {
		fixture := newLandingPlanFixture(t)
		fixture.input.CoverageComplete = false
		plan, err := PlanLanding(fixture.input)
		if err != nil {
			t.Fatalf("PlanLanding() error = %v", err)
		}
		if plan.Outcome != ContextPlanBlocked || len(plan.Conflicts) != 1 || plan.Conflicts[0].Reason != "coverage_incomplete" {
			t.Fatalf("PlanLanding() = %+v, want coverage blocked", plan)
		}
	})
}

func TestPlanLandingAppliesCandidateOperations(t *testing.T) {
	tests := []struct {
		name         string
		record       func(landingPlanFixture) CandidateContextRecord
		prepare      func(*landingPlanFixture)
		wantOutcome  ContextPlanOutcome
		wantNotes    int
		wantRevision string
		wantEntity   string
		wantConflict string
	}{
		{
			name: "add",
			record: func(f landingPlanFixture) CandidateContextRecord {
				return candidateRecord(CandidateAdd, "added", "added-revision", "", f.input.TargetEntities[0], "added body", f.note.CreatedAt.Add(time.Second))
			},
			wantOutcome:  ContextPlanReady,
			wantNotes:    2,
			wantRevision: "added-revision",
			wantEntity:   "example.Run",
		},
		{
			name: "identical add is no-op",
			record: func(f landingPlanFixture) CandidateContextRecord {
				return candidateRecord(CandidateAdd, f.note.ID, f.note.RevisionID, "", f.input.TargetEntities[0], f.note.Body, f.note.CreatedAt)
			},
			wantOutcome:  ContextPlanReady,
			wantNotes:    1,
			wantRevision: "base-revision",
			wantEntity:   "example.Run",
		},
		{
			name: "add collision conflicts",
			record: func(f landingPlanFixture) CandidateContextRecord {
				return candidateRecord(CandidateAdd, f.note.ID, f.note.RevisionID, "", f.input.TargetEntities[0], "different body", f.note.CreatedAt)
			},
			wantOutcome:  ContextPlanBlocked,
			wantNotes:    1,
			wantConflict: "note_collision",
		},
		{
			name: "revise",
			record: func(f landingPlanFixture) CandidateContextRecord {
				record := candidateRecord(CandidateRevise, f.note.ID, "next-revision", f.note.RevisionID, f.input.TargetEntities[0], "revised body", f.note.CreatedAt.Add(time.Second))
				record.SupersedesRevisionID = f.note.RevisionID
				return record
			},
			wantOutcome:  ContextPlanReady,
			wantNotes:    1,
			wantRevision: "next-revision",
			wantEntity:   "example.Run",
		},
		{
			name: "identical revise is no-op",
			prepare: func(f *landingPlanFixture) {
				f.note.RevisionID = "next-revision"
				f.note.SupersedesRevisionID = "base-revision"
				f.note.Body = "revised body"
				f.note.CreatedAt = f.note.CreatedAt.Add(time.Second)
				f.input.Canonical.Notes[0] = f.note
				f.input.Canonical.RevisionMappings[0].RevisionID = f.note.RevisionID
			},
			record: func(f landingPlanFixture) CandidateContextRecord {
				record := candidateRecord(CandidateRevise, f.note.ID, f.note.RevisionID, "base-revision", f.input.TargetEntities[0], f.note.Body, f.note.CreatedAt)
				record.SupersedesRevisionID = "base-revision"
				return record
			},
			wantOutcome:  ContextPlanReady,
			wantNotes:    1,
			wantRevision: "next-revision",
			wantEntity:   "example.Run",
		},
		{
			name: "stale revise conflicts",
			record: func(f landingPlanFixture) CandidateContextRecord {
				record := candidateRecord(CandidateRevise, f.note.ID, "next-revision", "stale-revision", f.input.TargetEntities[0], "revised body", f.note.CreatedAt.Add(time.Second))
				record.SupersedesRevisionID = "stale-revision"
				return record
			},
			wantOutcome:  ContextPlanBlocked,
			wantNotes:    1,
			wantConflict: "revision_conflict",
		},
		{
			name: "rebind",
			prepare: func(f *landingPlanFixture) {
				f.input.TargetEntities[0].Key = "moved.Run"
				f.input.Fingerprint.SourceEvidenceDigest = DigestSourceEvidence(f.input.TargetEntities)
			},
			record: func(f landingPlanFixture) CandidateContextRecord {
				return candidateRecord(CandidateRebind, f.note.ID, f.note.RevisionID, f.note.RevisionID, f.input.TargetEntities[0], f.note.Body, f.note.CreatedAt)
			},
			wantOutcome:  ContextPlanReady,
			wantNotes:    1,
			wantRevision: "base-revision",
			wantEntity:   "moved.Run",
		},
		{
			name: "rebind immutable mutation conflicts",
			prepare: func(f *landingPlanFixture) {
				f.input.TargetEntities[0].Key = "moved.Run"
				f.input.Fingerprint.SourceEvidenceDigest = DigestSourceEvidence(f.input.TargetEntities)
			},
			record: func(f landingPlanFixture) CandidateContextRecord {
				return candidateRecord(CandidateRebind, f.note.ID, f.note.RevisionID, f.note.RevisionID, f.input.TargetEntities[0], "mutated body", f.note.CreatedAt)
			},
			wantOutcome:  ContextPlanBlocked,
			wantNotes:    1,
			wantConflict: "immutable_revision_conflict",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLandingPlanFixture(t)
			if test.prepare != nil {
				test.prepare(&fixture)
			}
			setCandidate(t, &fixture.input, []CandidateContextRecord{test.record(fixture)})
			plan, err := PlanLanding(fixture.input)
			if err != nil {
				t.Fatalf("PlanLanding() error = %v", err)
			}
			if plan.Outcome != test.wantOutcome || len(plan.Decisions) != test.wantNotes {
				t.Fatalf("PlanLanding() = %+v, want outcome=%q notes=%d", plan, test.wantOutcome, test.wantNotes)
			}
			if test.wantConflict != "" {
				if len(plan.Conflicts) != 1 || plan.Conflicts[0].Reason != test.wantConflict {
					t.Fatalf("PlanLanding() conflicts = %+v, want %q", plan.Conflicts, test.wantConflict)
				}
				return
			}
			if len(plan.Conflicts) != 0 {
				t.Fatalf("PlanLanding() conflicts = %+v, want none", plan.Conflicts)
			}
			found := false
			for _, decision := range plan.Decisions {
				if decision.Note.RevisionID == test.wantRevision && decision.Note.EntityKey == test.wantEntity {
					found = true
				}
			}
			if !found {
				t.Fatalf("PlanLanding() decisions = %+v, want revision=%q entity=%q", plan.Decisions, test.wantRevision, test.wantEntity)
			}
		})
	}
}

func TestPlanLandingIDsAreDeterministicAcrossPlanningAttempts(t *testing.T) {
	fixture := newLandingPlanFixture(t)
	first, err := PlanLanding(fixture.input)
	if err != nil {
		t.Fatalf("PlanLanding(first) error = %v", err)
	}
	fixture.input.CreatedAt = fixture.input.CreatedAt.Add(time.Hour)
	second, err := PlanLanding(fixture.input)
	if err != nil {
		t.Fatalf("PlanLanding(second) error = %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("plan IDs differ across attempts: %q != %q", first.ID, second.ID)
	}
	if LandingIntentID(first.Fingerprint) != LandingIntentID(second.Fingerprint) {
		t.Fatalf("landing intent IDs differ for the same logical landing")
	}
}

func TestBuildLandingSnapshotProducesCanonicalV4Receipt(t *testing.T) {
	fixture := newLandingPlanFixture(t)
	record := candidateRecord(CandidateAdd, "added", "added-revision", "", fixture.input.TargetEntities[0], "added body", fixture.note.CreatedAt.Add(time.Second))
	setCandidate(t, &fixture.input, []CandidateContextRecord{record})
	plan, err := PlanLanding(fixture.input)
	if err != nil {
		t.Fatalf("PlanLanding() error = %v", err)
	}
	object, err := BuildLandingSnapshot(LandingBuildInput{
		Plan:       plan,
		ParentID:   fixture.input.Fingerprint.BaseContextCommitID,
		Entities:   fixture.input.TargetEntities,
		Provenance: fixture.input.TargetProvenance,
		Message:    "land context",
		Author:     "thread-keep",
		CreatedAt:  fixture.input.CreatedAt,
		Resolver:   "automatic",
	})
	if err != nil {
		t.Fatalf("BuildLandingSnapshot() error = %v", err)
	}
	if object.SchemaVersion != 4 || len(object.ParentIDs) != 1 || len(object.LandingReceipts) != 1 || len(object.Notes) != 2 || len(object.RevisionMappings) != 2 {
		t.Fatalf("BuildLandingSnapshot() = %+v", object)
	}
	receipt := object.LandingReceipts[0]
	if receipt.ID != LandingIntentID(plan.Fingerprint) || receipt.FinalPlanID != plan.ID || len(receipt.CandidateMappings) != 1 || receipt.CandidateMappings[0].CandidateRecordID == "" {
		t.Fatalf("landing receipt = %+v", receipt)
	}
	changedEntities := append([]Entity(nil), fixture.input.TargetEntities...)
	changedEntities[0].StructuralHash = strings.Repeat("8", 64)
	if _, err := BuildLandingSnapshot(LandingBuildInput{Plan: plan, ParentID: fixture.input.Fingerprint.BaseContextCommitID, Entities: changedEntities, Provenance: fixture.input.TargetProvenance, Message: "land context", Author: "thread-keep", CreatedAt: fixture.input.CreatedAt, Resolver: "automatic"}); CodeOf(err) != CodeValidation {
		t.Fatalf("BuildLandingSnapshot(changed evidence) error = %v, want validation", err)
	}
}

func TestNormalizeCandidateV1IsDeterministic(t *testing.T) {
	createdAt := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	candidate := Candidate{ID: "github:owner/repository#42", Provider: "github", Repository: "owner/repository", Number: 42, State: CandidateOpen, BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40), UpdatedAt: createdAt}
	notes := []CandidateNote{{CandidateID: candidate.ID, ID: "draft", EntityKey: "example.Run", StructuralHash: strings.Repeat("3", 64), Kind: NoteIntent, Body: "candidate context", Author: "tester", Origin: "provider", CreatedAt: createdAt, State: CandidateNoteDraft}}
	first, err := NormalizeCandidateV1(candidate, notes, strings.Repeat("4", 64))
	if err != nil {
		t.Fatalf("NormalizeCandidateV1(first) error = %v", err)
	}
	second, err := NormalizeCandidateV1(candidate, notes, strings.Repeat("4", 64))
	if err != nil {
		t.Fatalf("NormalizeCandidateV1(second) error = %v", err)
	}
	firstDigest, err := CandidateContextDigest(first)
	if err != nil {
		t.Fatalf("CandidateContextDigest(first) error = %v", err)
	}
	secondDigest, err := CandidateContextDigest(second)
	if err != nil {
		t.Fatalf("CandidateContextDigest(second) error = %v", err)
	}
	if !reflect.DeepEqual(first, second) || firstDigest != secondDigest || first.Records[0].ID == "" || first.Records[0].NoteID == notes[0].ID {
		t.Fatalf("candidate normalization is not deterministic: first=%+v second=%+v", first, second)
	}
}

func FuzzCandidateContextDigestDeterministic(f *testing.F) {
	f.Add("candidate body")
	f.Fuzz(func(t *testing.T, body string) {
		if body == "" || len(body) > 1024 {
			return
		}
		fixture := newLandingPlanFixture(t)
		record := candidateRecord(CandidateAdd, "fuzz-note", "fuzz-revision", "", fixture.input.TargetEntities[0], body, fixture.note.CreatedAt)
		delta := CandidateContextDelta{SchemaVersion: 2, Change: fixture.input.Fingerprint.Change, BaseSourceSHA: fixture.input.Fingerprint.BaseSourceSHA, HeadSourceSHA: fixture.input.Fingerprint.HeadSourceSHA, BaseContextCommitID: fixture.input.Fingerprint.BaseContextCommitID, Records: []CandidateContextRecord{record}}
		first, err := NormalizeCandidateContextDelta(delta)
		if err != nil {
			return
		}
		second, err := NormalizeCandidateContextDelta(delta)
		if err != nil {
			t.Fatalf("second normalization error = %v", err)
		}
		firstDigest, err := CandidateContextDigest(first)
		if err != nil {
			t.Fatalf("first digest error = %v", err)
		}
		secondDigest, err := CandidateContextDigest(second)
		if err != nil {
			t.Fatalf("second digest error = %v", err)
		}
		if firstDigest != secondDigest || !reflect.DeepEqual(first, second) {
			t.Fatalf("normalization or digest is nondeterministic")
		}
	})
}

func newLandingPlanFixture(t testing.TB) landingPlanFixture {
	t.Helper()
	baseSource := strings.Repeat("1", 40)
	targetSource := strings.Repeat("2", 40)
	structuralHash := strings.Repeat("3", 64)
	createdAt := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	baseEntity := Entity{Language: "go", Key: "example.Run", Kind: EntityFunction, Name: "Run", Path: "example.go", SourceSHA: baseSource, StructuralHash: structuralHash}
	targetEntity := baseEntity
	targetEntity.SourceSHA = targetSource
	note := Note{ID: "base-note", RevisionID: "base-revision", EntityKey: baseEntity.Key, Kind: NoteIntent, Body: "base body", Author: "tester", Origin: "human", CreatedAt: createdAt, BindingState: NoteBindingActive, BindingSourceSHA: baseSource}
	provenance := []ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: targetSource}}
	change := ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}
	input := LandingPlanInput{
		Kind: ContextPlanFinal,
		Fingerprint: PlanFingerprint{
			RepositoryID:         "git-roots:example",
			TargetRef:            "refs/contexts/main",
			Change:               change,
			BaseSourceSHA:        baseSource,
			HeadSourceSHA:        targetSource,
			SourceEvidenceDigest: DigestSourceEvidence([]Entity{targetEntity}),
			BaseContextCommitID:  strings.Repeat("4", 64),
			BaseContextVersion:   7,
			ProvenanceDigest:     DigestProvenance(provenance),
		},
		Canonical: ContextObject{
			SchemaVersion:    3,
			RepositoryID:     "git-roots:example",
			RefName:          "refs/contexts/main",
			SourceSHA:        baseSource,
			Provenance:       []ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: baseSource}},
			Entities:         []Entity{baseEntity},
			Notes:            []Note{note},
			RevisionMappings: []ContextRevisionMapping{{EntityKey: note.EntityKey, NoteID: note.ID, RevisionID: note.RevisionID, BindingState: note.BindingState, BindingSourceSHA: note.BindingSourceSHA}},
		},
		TargetEntities:   []Entity{targetEntity},
		TargetProvenance: provenance,
		CoverageComplete: true,
		CreatedAt:        createdAt,
	}
	return landingPlanFixture{input: input, note: note}
}

func candidateRecord(operation CandidateContextOperation, noteID, revisionID, baseRevisionID string, entity Entity, body string, createdAt time.Time) CandidateContextRecord {
	return CandidateContextRecord{
		Operation:      operation,
		NoteID:         noteID,
		RevisionID:     revisionID,
		BaseRevisionID: baseRevisionID,
		EntityKey:      entity.Key,
		StructuralHash: entity.StructuralHash,
		Kind:           NoteIntent,
		Body:           body,
		Author:         "tester",
		Origin:         "human",
		CreatedAt:      createdAt,
	}
}

func setCandidate(t testing.TB, input *LandingPlanInput, records []CandidateContextRecord) {
	t.Helper()
	delta, err := NormalizeCandidateContextDelta(CandidateContextDelta{
		SchemaVersion:       2,
		Change:              input.Fingerprint.Change,
		BaseSourceSHA:       input.Fingerprint.BaseSourceSHA,
		HeadSourceSHA:       input.Fingerprint.HeadSourceSHA,
		BaseContextCommitID: input.Fingerprint.BaseContextCommitID,
		Records:             records,
	})
	if err != nil {
		t.Fatalf("NormalizeCandidateContextDelta() error = %v", err)
	}
	digest, err := CandidateContextDigest(delta)
	if err != nil {
		t.Fatalf("CandidateContextDigest() error = %v", err)
	}
	input.Candidate = delta
	input.Fingerprint.CandidateDigest = digest
}
