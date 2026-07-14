package domain

import (
	"errors"
	"sort"
	"strings"
	"time"
)

type LandingSessionState string

const (
	LandingSessionOpen      LandingSessionState = "open"
	LandingSessionReady     LandingSessionState = "ready"
	LandingSessionCommitted LandingSessionState = "committed"
)

type LandingResolutionUse string

const (
	LandingUseCanonical LandingResolutionUse = "canonical"
	LandingUseCandidate LandingResolutionUse = "candidate"
	LandingUseAuthored  LandingResolutionUse = "authored"
)

type LandingSession struct {
	ID                       string                      `json:"id"`
	Version                  int                         `json:"version"`
	LandingID                string                      `json:"landing_id"`
	RemoteName               string                      `json:"remote_name"`
	RepositoryID             string                      `json:"repository_id"`
	RefName                  string                      `json:"ref_name"`
	SourceSHA                string                      `json:"source_sha"`
	ExpectedRemoteCommitID   string                      `json:"expected_remote_commit_id"`
	ExpectedRemoteRefVersion int                         `json:"expected_remote_ref_version"`
	Plan                     ContextPlan                 `json:"plan"`
	Candidate                CandidateContextDelta       `json:"candidate"`
	Entities                 []Entity                    `json:"entities"`
	Provenance               []ContextSnapshotProvenance `json:"provenance"`
	State                    LandingSessionState         `json:"state"`
	CreatedAt                time.Time                   `json:"created_at"`
}

func LandingConflictID(conflict LandingConflict) string {
	return digestParts("landing-conflict", conflict.CandidateRecordID, conflict.NoteID, conflict.Reason, conflict.BaseRevisionID, conflict.CanonicalRevisionID)
}

func ResolveLandingPlan(plan ContextPlan, candidate CandidateContextDelta, entities []Entity, conflictID string, use LandingResolutionUse, authored *Note) (ContextPlan, error) {
	index := -1
	for candidateIndex, conflict := range plan.Conflicts {
		if LandingConflictID(conflict) == conflictID {
			index = candidateIndex
			break
		}
	}
	if index < 0 {
		return ContextPlan{}, NewError(CodeEntityNotFound, errors.New("landing conflict does not exist"))
	}
	conflict := plan.Conflicts[index]
	switch use {
	case LandingUseCanonical:
	case LandingUseCandidate:
		record, found := candidateRecordByID(candidate, conflict.CandidateRecordID)
		if !found {
			return ContextPlan{}, NewError(CodeValidation, errors.New("landing conflict has no candidate record"))
		}
		note := noteFromCandidateRecord(record, entities, plan.Fingerprint.HeadSourceSHA)
		plan.Decisions = upsertLandingDecision(plan.Decisions, landingDecision(record.ID, record.Operation, note, false))
	case LandingUseAuthored:
		if authored == nil || authored.ID != conflict.NoteID || authored.RevisionID == "" || authored.EntityKey == "" || !ValidNoteKind(authored.Kind) || strings.TrimSpace(authored.Body) == "" || authored.Author == "" || authored.Origin == "" || authored.CreatedAt.IsZero() || !ValidNoteBindingState(authored.BindingState) || authored.BindingSourceSHA != plan.Fingerprint.HeadSourceSHA || authored.Pending {
			return ContextPlan{}, NewError(CodeValidation, errors.New("authored landing resolution is invalid"))
		}
		plan.Decisions = upsertLandingDecision(plan.Decisions, landingDecision("", "", *authored, false))
	default:
		return ContextPlan{}, NewError(CodeValidation, errors.New("landing resolution choice is invalid"))
	}
	plan.Conflicts = append(plan.Conflicts[:index:index], plan.Conflicts[index+1:]...)
	refreshLandingPlan(&plan)
	identifier, err := contextPlanID(plan)
	if err != nil {
		return ContextPlan{}, err
	}
	plan.ID = identifier
	return plan, nil
}

func candidateRecordByID(candidate CandidateContextDelta, identifier string) (CandidateContextRecord, bool) {
	for _, record := range candidate.Records {
		if record.ID == identifier {
			return record, true
		}
	}
	return CandidateContextRecord{}, false
}

func upsertLandingDecision(decisions []LandingDecision, next LandingDecision) []LandingDecision {
	for index := range decisions {
		if decisions[index].Note.ID == next.Note.ID {
			decisions[index] = next
			return decisions
		}
	}
	return append(decisions, next)
}

func refreshLandingPlan(plan *ContextPlan) {
	sort.Slice(plan.Decisions, func(i, j int) bool { return plan.Decisions[i].Note.ID < plan.Decisions[j].Note.ID })
	plan.Summary.ActiveNotes = 0
	plan.Summary.NeedsReviewNotes = 0
	plan.Summary.HistoricalNotes = 0
	plan.Summary.Conflicts = len(plan.Conflicts)
	for _, decision := range plan.Decisions {
		switch decision.Note.BindingState {
		case NoteBindingActive:
			plan.Summary.ActiveNotes++
		case NoteBindingNeedsReview:
			plan.Summary.NeedsReviewNotes++
		case NoteBindingHistorical:
			plan.Summary.HistoricalNotes++
		}
	}
	switch {
	case len(plan.Conflicts) != 0:
		plan.Outcome = ContextPlanBlocked
	case plan.Summary.NeedsReviewNotes != 0 || plan.Summary.HistoricalNotes != 0:
		plan.Outcome = ContextPlanReviewRequired
	default:
		plan.Outcome = ContextPlanReady
	}
}
