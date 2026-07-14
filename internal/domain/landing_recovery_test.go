package domain

import (
	"strings"
	"testing"
	"time"
)

func TestResolveLandingPlanSupportsCanonicalCandidateAndAuthoredChoices(t *testing.T) {
	now := time.Now().UTC()
	entity := Entity{Language: "go", Key: "example.Value", Kind: EntityFunction, Name: "Value", Path: "main.go", StartLine: 1, EndLine: 1, SourceSHA: strings.Repeat("2", 40), StructuralHash: strings.Repeat("3", 64)}
	record := CandidateContextRecord{Operation: CandidateAdd, NoteID: "note-1", RevisionID: "revision-candidate", EntityKey: entity.Key, StructuralHash: entity.StructuralHash, Kind: NoteIntent, Body: "candidate", Author: "author", Origin: "human", CreatedAt: now}
	normalized, err := NormalizeCandidateContextDelta(CandidateContextDelta{SchemaVersion: 2, Change: ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}, BaseSourceSHA: strings.Repeat("1", 40), HeadSourceSHA: strings.Repeat("2", 40), Records: []CandidateContextRecord{record}})
	if err != nil {
		t.Fatalf("NormalizeCandidateContextDelta() error = %v", err)
	}
	conflict := LandingConflict{CandidateRecordID: normalized.Records[0].ID, NoteID: "note-1", Reason: "note_collision"}
	plan := ContextPlan{SchemaVersion: 1, Kind: ContextPlanFinal, Fingerprint: PlanFingerprint{RepositoryID: "repo", TargetRef: "refs/contexts/main", Change: normalized.Change, BaseSourceSHA: normalized.BaseSourceSHA, HeadSourceSHA: normalized.HeadSourceSHA}, Outcome: ContextPlanBlocked, Conflicts: []LandingConflict{conflict}, Summary: LandingSummary{Conflicts: 1}, CreatedAt: now}
	resolved, err := ResolveLandingPlan(plan, normalized, []Entity{entity}, LandingConflictID(conflict), LandingUseCandidate, nil)
	if err != nil || resolved.Outcome != ContextPlanReady || len(resolved.Conflicts) != 0 || len(resolved.Decisions) != 1 || resolved.Decisions[0].Note.Body != "candidate" || resolved.ID == "" {
		t.Fatalf("ResolveLandingPlan(candidate) = %+v, %v", resolved, err)
	}
	authored := resolved.Decisions[0].Note
	authored.Body = "authored"
	authored.RevisionID = "revision-authored"
	authoredPlan, err := ResolveLandingPlan(plan, normalized, []Entity{entity}, LandingConflictID(conflict), LandingUseAuthored, &authored)
	if err != nil || authoredPlan.Decisions[0].Note.Body != "authored" {
		t.Fatalf("ResolveLandingPlan(authored) = %+v, %v", authoredPlan, err)
	}
	canonical, err := ResolveLandingPlan(plan, normalized, []Entity{entity}, LandingConflictID(conflict), LandingUseCanonical, nil)
	if err != nil || canonical.Outcome != ContextPlanReady || len(canonical.Decisions) != 0 {
		t.Fatalf("ResolveLandingPlan(canonical) = %+v, %v", canonical, err)
	}
}
