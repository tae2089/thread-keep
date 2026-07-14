package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zeebo/blake3"
)

type ChangeKey struct {
	Provider   string `json:"provider"`
	Repository string `json:"repository"`
	Number     int    `json:"number"`
}

type CandidateContextOperation string

const (
	CandidateAdd    CandidateContextOperation = "add"
	CandidateRevise CandidateContextOperation = "revise"
	CandidateRebind CandidateContextOperation = "rebind"
)

type CandidateContextRecord struct {
	ID                   string                    `json:"id"`
	Operation            CandidateContextOperation `json:"operation"`
	NoteID               string                    `json:"note_id"`
	RevisionID           string                    `json:"revision_id"`
	BaseRevisionID       string                    `json:"base_revision_id,omitempty"`
	SupersedesRevisionID string                    `json:"supersedes_revision_id,omitempty"`
	EntityKey            string                    `json:"entity_key"`
	StructuralHash       string                    `json:"structural_hash"`
	Kind                 NoteKind                  `json:"kind"`
	Body                 string                    `json:"body"`
	Topics               []string                  `json:"topics,omitempty"`
	Author               string                    `json:"author"`
	Origin               string                    `json:"origin"`
	CreatedAt            time.Time                 `json:"created_at"`
}

type CandidateContextDelta struct {
	SchemaVersion       int                      `json:"schema_version"`
	Change              ChangeKey                `json:"change"`
	BaseSourceSHA       string                   `json:"base_source_sha"`
	HeadSourceSHA       string                   `json:"head_source_sha"`
	BaseContextCommitID string                   `json:"base_context_commit_id,omitempty"`
	Records             []CandidateContextRecord `json:"records"`
}

type ContextPlanKind string

const (
	ContextPlanPreview ContextPlanKind = "preview"
	ContextPlanFinal   ContextPlanKind = "final"
)

type ContextPlanOutcome string

const (
	ContextPlanReady          ContextPlanOutcome = "ready"
	ContextPlanReviewRequired ContextPlanOutcome = "review_required"
	ContextPlanBlocked        ContextPlanOutcome = "blocked"
)

type PlanFingerprint struct {
	RepositoryID         string    `json:"repository_id"`
	TargetRef            string    `json:"target_ref"`
	Change               ChangeKey `json:"change"`
	BaseSourceSHA        string    `json:"base_source_sha"`
	HeadSourceSHA        string    `json:"head_source_sha"`
	SourceEvidenceDigest string    `json:"source_evidence_digest"`
	BaseContextCommitID  string    `json:"base_context_commit_id,omitempty"`
	BaseContextVersion   int       `json:"base_context_version"`
	CandidateDigest      string    `json:"candidate_digest,omitempty"`
	ProvenanceDigest     string    `json:"provenance_digest"`
}

type LandingDecision struct {
	CandidateRecordID string                    `json:"candidate_record_id,omitempty"`
	Operation         CandidateContextOperation `json:"operation,omitempty"`
	Note              Note                      `json:"note"`
	Mapping           ContextRevisionMapping    `json:"mapping"`
	NoOp              bool                      `json:"no_op,omitempty"`
}

type LandingConflict struct {
	CandidateRecordID   string `json:"candidate_record_id,omitempty"`
	NoteID              string `json:"note_id,omitempty"`
	Reason              string `json:"reason"`
	BaseRevisionID      string `json:"base_revision_id,omitempty"`
	CanonicalRevisionID string `json:"canonical_revision_id,omitempty"`
}

type LandingSummary struct {
	ActiveNotes      int `json:"active_notes"`
	NeedsReviewNotes int `json:"needs_review_notes"`
	HistoricalNotes  int `json:"historical_notes"`
	CandidateRecords int `json:"candidate_records"`
	Conflicts        int `json:"conflicts"`
}

type ContextPlan struct {
	SchemaVersion int                `json:"schema_version"`
	ID            string             `json:"id"`
	Kind          ContextPlanKind    `json:"kind"`
	Fingerprint   PlanFingerprint    `json:"fingerprint"`
	Outcome       ContextPlanOutcome `json:"outcome"`
	EntityChanges []EntityChange     `json:"entity_changes,omitempty"`
	Decisions     []LandingDecision  `json:"decisions"`
	Conflicts     []LandingConflict  `json:"conflicts,omitempty"`
	Summary       LandingSummary     `json:"summary"`
	CreatedAt     time.Time          `json:"created_at"`
}

type LandingPlanInput struct {
	Kind             ContextPlanKind
	Fingerprint      PlanFingerprint
	Canonical        ContextObject
	TargetEntities   []Entity
	TargetProvenance []ContextSnapshotProvenance
	CoverageComplete bool
	Candidate        CandidateContextDelta
	CreatedAt        time.Time
}

type LandingBuildInput struct {
	Plan       ContextPlan
	ParentID   string
	Entities   []Entity
	Provenance []ContextSnapshotProvenance
	Message    string
	Author     string
	CreatedAt  time.Time
	Resolver   string
}

type LandingState string

const (
	LandingPending    LandingState = "pending"
	LandingRunning    LandingState = "running"
	LandingRetryable  LandingState = "retryable"
	LandingBlocked    LandingState = "blocked"
	LandingRecovering LandingState = "recovering"
	LandingLanded     LandingState = "landed"
)

type LandingIntent struct {
	ID                    string       `json:"id"`
	RepositoryID          string       `json:"repository_id"`
	TargetRef             string       `json:"target_ref"`
	Change                ChangeKey    `json:"change"`
	CandidateDigest       string       `json:"candidate_digest,omitempty"`
	SourceMergeSHA        string       `json:"source_merge_sha"`
	PreviewPlanID         string       `json:"preview_plan_id,omitempty"`
	FinalPlanID           string       `json:"final_plan_id,omitempty"`
	State                 LandingState `json:"state"`
	AttemptCount          int          `json:"attempt_count"`
	NextAttemptAt         time.Time    `json:"next_attempt_at,omitempty"`
	LastErrorCode         ErrorCode    `json:"last_error_code,omitempty"`
	LandedContextCommitID string       `json:"landed_context_commit_id,omitempty"`
}

func NormalizeCandidateContextDelta(input CandidateContextDelta) (CandidateContextDelta, error) {
	if input.SchemaVersion != 2 {
		return CandidateContextDelta{}, NewError(CodeValidation, fmt.Errorf("unsupported candidate context delta schema version %d", input.SchemaVersion))
	}
	change, err := normalizeChangeKey(input.Change)
	if err != nil {
		return CandidateContextDelta{}, err
	}
	if !validCandidateSHA(input.BaseSourceSHA) || !validCandidateSHA(input.HeadSourceSHA) {
		return CandidateContextDelta{}, NewError(CodeValidation, errors.New("candidate context delta source revisions are invalid"))
	}
	baseContextID := strings.ToLower(strings.TrimSpace(input.BaseContextCommitID))
	if baseContextID != "" {
		normalized, err := NormalizeContextCommitID(baseContextID)
		if err != nil || normalized != baseContextID {
			return CandidateContextDelta{}, NewError(CodeValidation, errors.New("candidate context delta base context ID is invalid"))
		}
	}
	if len(input.Records) > maxCandidateNotes {
		return CandidateContextDelta{}, NewError(CodeValidation, fmt.Errorf("candidate context delta exceeds %d records", maxCandidateNotes))
	}
	records := make([]CandidateContextRecord, 0, len(input.Records))
	noteIDs := make(map[string]struct{}, len(input.Records))
	for _, raw := range input.Records {
		record, err := normalizeCandidateContextRecord(raw)
		if err != nil {
			return CandidateContextDelta{}, err
		}
		if _, found := noteIDs[record.NoteID]; found {
			return CandidateContextDelta{}, NewError(CodeValidation, errors.New("candidate context delta contains multiple records for one note"))
		}
		noteIDs[record.NoteID] = struct{}{}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	for index := 1; index < len(records); index++ {
		if records[index-1].ID == records[index].ID {
			return CandidateContextDelta{}, NewError(CodeValidation, errors.New("candidate context delta contains duplicate record IDs"))
		}
	}
	return CandidateContextDelta{SchemaVersion: 2, Change: change, BaseSourceSHA: strings.ToLower(input.BaseSourceSHA), HeadSourceSHA: strings.ToLower(input.HeadSourceSHA), BaseContextCommitID: baseContextID, Records: records}, nil
}

func ParseChangeKey(value string) (ChangeKey, error) {
	value = strings.TrimSpace(value)
	separator := strings.IndexByte(value, ':')
	numberSeparator := strings.LastIndexByte(value, '#')
	if separator < 1 || numberSeparator <= separator+1 || numberSeparator == len(value)-1 {
		return ChangeKey{}, NewError(CodeValidation, errors.New("change key must use provider:repository#number"))
	}
	number, err := strconv.Atoi(value[numberSeparator+1:])
	if err != nil {
		return ChangeKey{}, NewError(CodeValidation, errors.New("change key number is invalid"))
	}
	return normalizeChangeKey(ChangeKey{Provider: value[:separator], Repository: value[separator+1 : numberSeparator], Number: number})
}

func CandidateContextDigest(input CandidateContextDelta) (string, error) {
	normalized, err := NormalizeCandidateContextDelta(input)
	if err != nil {
		return "", err
	}
	return canonicalDigest(normalized)
}

func NormalizeCandidateV1(candidate Candidate, notes []CandidateNote, baseContextCommitID string) (CandidateContextDelta, error) {
	change, err := normalizeChangeKey(ChangeKey{Provider: candidate.Provider, Repository: candidate.Repository, Number: candidate.Number})
	if err != nil {
		return CandidateContextDelta{}, err
	}
	if candidate.ID != change.Provider+":"+change.Repository+"#"+fmt.Sprint(change.Number) || !validCandidateSHA(candidate.BaseSHA) || !validCandidateSHA(candidate.HeadSHA) {
		return CandidateContextDelta{}, NewError(CodeValidation, errors.New("candidate v1 identity or source revisions are invalid"))
	}
	records := make([]CandidateContextRecord, 0, len(notes))
	for _, note := range notes {
		if note.CandidateID != candidate.ID || note.State != CandidateNoteDraft || note.PromotedNoteID != "" {
			return CandidateContextDelta{}, NewError(CodeValidation, errors.New("candidate v1 note is not a draft for the candidate"))
		}
		if err := validateCandidateNote(note); err != nil {
			return CandidateContextDelta{}, err
		}
		records = append(records, CandidateContextRecord{
			Operation:      CandidateAdd,
			NoteID:         digestParts("candidate-v1-note", candidate.ID, note.ID),
			RevisionID:     digestParts("candidate-v1-revision", candidate.ID, note.ID),
			EntityKey:      note.EntityKey,
			StructuralHash: strings.ToLower(note.StructuralHash),
			Kind:           note.Kind,
			Body:           note.Body,
			Author:         note.Author,
			Origin:         note.Origin,
			CreatedAt:      note.CreatedAt.UTC(),
		})
	}
	return NormalizeCandidateContextDelta(CandidateContextDelta{SchemaVersion: 2, Change: change, BaseSourceSHA: candidate.BaseSHA, HeadSourceSHA: candidate.HeadSHA, BaseContextCommitID: baseContextCommitID, Records: records})
}

func DigestSourceEvidence(entities []Entity) string {
	canonical := append([]Entity(nil), entities...)
	for index := range canonical {
		canonical[index].SourceSHA = ""
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Key < canonical[j].Key })
	digest, _ := canonicalDigest(canonical)
	return digest
}

func DigestProvenance(provenance []ContextSnapshotProvenance) string {
	canonical := append([]ContextSnapshotProvenance(nil), provenance...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Language < canonical[j].Language })
	digest, _ := canonicalDigest(canonical)
	return digest
}

func LandingIntentID(fingerprint PlanFingerprint) string {
	return digestParts("landing-intent", fingerprint.RepositoryID, fingerprint.TargetRef, fingerprint.Change.Provider, fingerprint.Change.Repository, fmt.Sprint(fingerprint.Change.Number), fingerprint.HeadSourceSHA, fingerprint.CandidateDigest)
}

func PlanLanding(input LandingPlanInput) (ContextPlan, error) {
	canonical, targetEntities, targetProvenance, candidate, err := validateLandingPlanInput(input)
	if err != nil {
		return ContextPlan{}, err
	}
	plan := ContextPlan{
		SchemaVersion: 1,
		Kind:          input.Kind,
		Fingerprint:   input.Fingerprint,
		EntityChanges: ClassifyEntityChanges(canonical.Entities, targetEntities),
		CreatedAt:     input.CreatedAt.UTC(),
	}
	decisions := make(map[string]LandingDecision, len(canonical.Notes)+len(candidate.Records))
	previousByKey := make(map[string]Entity, len(canonical.Entities))
	for _, entity := range canonical.Entities {
		previousByKey[entity.Key] = entity
	}
	reconciler := NewBindingReconciler(targetEntities, input.Fingerprint.HeadSourceSHA)
	for _, note := range canonical.Notes {
		updated, _ := reconciler.Reconcile(note, previousByKey[note.EntityKey])
		updated.Pending = false
		decisions[note.ID] = landingDecision("", "", updated, false)
	}
	if !input.CoverageComplete {
		plan.Conflicts = append(plan.Conflicts, LandingConflict{Reason: "coverage_incomplete"})
	}
	if input.CoverageComplete && len(targetProvenance) == 0 {
		plan.Conflicts = append(plan.Conflicts, LandingConflict{Reason: "provenance_incomplete"})
	}
	for _, record := range candidate.Records {
		applyCandidateRecord(record, decisions, targetEntities, input.Fingerprint.HeadSourceSHA, &plan.Conflicts)
	}
	plan.Decisions = make([]LandingDecision, 0, len(decisions))
	for _, decision := range decisions {
		plan.Decisions = append(plan.Decisions, decision)
		switch decision.Note.BindingState {
		case NoteBindingActive:
			plan.Summary.ActiveNotes++
		case NoteBindingNeedsReview:
			plan.Summary.NeedsReviewNotes++
		case NoteBindingHistorical:
			plan.Summary.HistoricalNotes++
		}
	}
	sort.Slice(plan.Decisions, func(i, j int) bool { return plan.Decisions[i].Note.ID < plan.Decisions[j].Note.ID })
	sort.Slice(plan.Conflicts, func(i, j int) bool {
		left := plan.Conflicts[i].CandidateRecordID + "\x00" + plan.Conflicts[i].NoteID + "\x00" + plan.Conflicts[i].Reason
		right := plan.Conflicts[j].CandidateRecordID + "\x00" + plan.Conflicts[j].NoteID + "\x00" + plan.Conflicts[j].Reason
		return left < right
	})
	plan.Summary.CandidateRecords = len(candidate.Records)
	plan.Summary.Conflicts = len(plan.Conflicts)
	switch {
	case len(plan.Conflicts) != 0:
		plan.Outcome = ContextPlanBlocked
	case plan.Summary.NeedsReviewNotes != 0 || plan.Summary.HistoricalNotes != 0:
		plan.Outcome = ContextPlanReviewRequired
	default:
		plan.Outcome = ContextPlanReady
	}
	plan.ID, err = contextPlanID(plan)
	if err != nil {
		return ContextPlan{}, err
	}
	return plan, nil
}

func BuildLandingSnapshot(input LandingBuildInput) (ContextObject, error) {
	if input.Plan.Kind != ContextPlanFinal || input.Plan.Outcome == ContextPlanBlocked || len(input.Plan.Conflicts) != 0 {
		return ContextObject{}, NewError(CodeValidation, errors.New("only an unblocked final context plan can build a landing snapshot"))
	}
	wantPlanID, err := contextPlanID(input.Plan)
	if err != nil || wantPlanID != input.Plan.ID {
		return ContextObject{}, NewError(CodeValidation, errors.New("landing plan ID does not match its canonical content"))
	}
	if input.ParentID != input.Plan.Fingerprint.BaseContextCommitID || strings.TrimSpace(input.Message) == "" || strings.TrimSpace(input.Author) == "" || strings.TrimSpace(input.Resolver) == "" || input.CreatedAt.IsZero() {
		return ContextObject{}, NewError(CodeValidation, errors.New("landing snapshot metadata is incomplete or stale"))
	}
	entities, err := normalizeLandingEntities(input.Entities, input.Plan.Fingerprint.HeadSourceSHA)
	if err != nil || DigestSourceEvidence(entities) != input.Plan.Fingerprint.SourceEvidenceDigest {
		return ContextObject{}, NewError(CodeValidation, errors.New("landing snapshot entities do not match the plan fingerprint"))
	}
	provenance, err := normalizeLandingProvenance(input.Provenance, input.Plan.Fingerprint.HeadSourceSHA)
	if err != nil || DigestProvenance(provenance) != input.Plan.Fingerprint.ProvenanceDigest {
		return ContextObject{}, NewError(CodeValidation, errors.New("landing snapshot provenance does not match the plan fingerprint"))
	}
	notes := make([]Note, 0, len(input.Plan.Decisions))
	mappings := make([]ContextRevisionMapping, 0, len(input.Plan.Decisions))
	candidateMappings := make([]CandidatePromotionMapping, 0)
	for _, decision := range input.Plan.Decisions {
		note := decision.Note
		if note.ID == "" || note.RevisionID == "" || note.BindingSourceSHA != input.Plan.Fingerprint.HeadSourceSHA || note.Pending || !ValidNoteBindingState(note.BindingState) {
			return ContextObject{}, NewError(CodeValidation, errors.New("landing plan contains an invalid note decision"))
		}
		notes = append(notes, note)
		mappings = append(mappings, decision.Mapping)
		if decision.CandidateRecordID != "" {
			candidateMappings = append(candidateMappings, CandidatePromotionMapping{CandidateRecordID: decision.CandidateRecordID, NoteID: note.ID, RevisionID: note.RevisionID})
		}
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].ID < notes[j].ID })
	sort.Slice(mappings, func(i, j int) bool {
		left := mappings[i].EntityKey + "\x00" + mappings[i].NoteID + "\x00" + mappings[i].RevisionID
		right := mappings[j].EntityKey + "\x00" + mappings[j].NoteID + "\x00" + mappings[j].RevisionID
		return left < right
	})
	sort.Slice(candidateMappings, func(i, j int) bool {
		left := candidateMappings[i].CandidateRecordID + "\x00" + candidateMappings[i].NoteID + "\x00" + candidateMappings[i].RevisionID
		right := candidateMappings[j].CandidateRecordID + "\x00" + candidateMappings[j].NoteID + "\x00" + candidateMappings[j].RevisionID
		return left < right
	})
	parentIDs := []string(nil)
	if input.ParentID != "" {
		parentIDs = []string{input.ParentID}
	}
	fingerprint := input.Plan.Fingerprint
	receipt := LandingReceipt{
		ID:                  LandingIntentID(fingerprint),
		Provider:            fingerprint.Change.Provider,
		ForgeRepository:     fingerprint.Change.Repository,
		ChangeNumber:        fingerprint.Change.Number,
		ContextRepositoryID: fingerprint.RepositoryID,
		TargetRef:           fingerprint.TargetRef,
		CandidateDigest:     fingerprint.CandidateDigest,
		FinalPlanID:         input.Plan.ID,
		SourceMergeSHA:      fingerprint.HeadSourceSHA,
		BaseContextCommitID: fingerprint.BaseContextCommitID,
		Resolver:            strings.TrimSpace(input.Resolver),
		CandidateMappings:   candidateMappings,
	}
	return ContextObject{
		SchemaVersion:    4,
		RepositoryID:     fingerprint.RepositoryID,
		RefName:          fingerprint.TargetRef,
		ParentIDs:        parentIDs,
		SourceSHA:        fingerprint.HeadSourceSHA,
		Message:          strings.TrimSpace(input.Message),
		Author:           strings.TrimSpace(input.Author),
		CreatedAt:        input.CreatedAt.UTC(),
		Provenance:       provenance,
		Entities:         entities,
		Notes:            notes,
		RevisionMappings: mappings,
		LandingReceipts:  []LandingReceipt{receipt},
	}, nil
}

func normalizeCandidateContextRecord(input CandidateContextRecord) (CandidateContextRecord, error) {
	record := input
	record.NoteID = strings.TrimSpace(record.NoteID)
	record.RevisionID = strings.TrimSpace(record.RevisionID)
	record.BaseRevisionID = strings.TrimSpace(record.BaseRevisionID)
	record.SupersedesRevisionID = strings.TrimSpace(record.SupersedesRevisionID)
	record.EntityKey = strings.TrimSpace(record.EntityKey)
	record.StructuralHash = strings.ToLower(strings.TrimSpace(record.StructuralHash))
	record.Author = strings.TrimSpace(record.Author)
	record.Origin = strings.TrimSpace(record.Origin)
	record.CreatedAt = record.CreatedAt.UTC()
	topics, err := NormalizeNoteTopics(record.Topics)
	if err != nil {
		return CandidateContextRecord{}, err
	}
	record.Topics = topics
	if record.NoteID == "" || record.RevisionID == "" || record.EntityKey == "" || !validCandidateSHA(record.StructuralHash) || !ValidNoteKind(record.Kind) || strings.TrimSpace(record.Body) == "" || len(record.Body) > 64*1024 || record.Author == "" || record.Origin == "" || record.CreatedAt.IsZero() {
		return CandidateContextRecord{}, NewError(CodeValidation, errors.New("candidate context record is incomplete or invalid"))
	}
	switch record.Operation {
	case CandidateAdd:
		if record.BaseRevisionID != "" || record.SupersedesRevisionID != "" {
			return CandidateContextRecord{}, NewError(CodeValidation, errors.New("candidate add must not declare a base or superseded revision"))
		}
	case CandidateRevise:
		if record.BaseRevisionID == "" || record.SupersedesRevisionID != record.BaseRevisionID || record.RevisionID == record.BaseRevisionID {
			return CandidateContextRecord{}, NewError(CodeValidation, errors.New("candidate revise must declare the current base as its superseded revision"))
		}
	case CandidateRebind:
		if record.BaseRevisionID == "" || record.RevisionID != record.BaseRevisionID || record.SupersedesRevisionID != "" {
			return CandidateContextRecord{}, NewError(CodeValidation, errors.New("candidate rebind must preserve the base revision"))
		}
	default:
		return CandidateContextRecord{}, NewError(CodeValidation, errors.New("candidate context record operation is invalid"))
	}
	providedID := strings.ToLower(strings.TrimSpace(record.ID))
	record.ID = ""
	identifier, err := canonicalDigest(record)
	if err != nil {
		return CandidateContextRecord{}, err
	}
	if providedID != "" && providedID != identifier {
		return CandidateContextRecord{}, NewError(CodeValidation, errors.New("candidate context record ID does not match its canonical content"))
	}
	record.ID = identifier
	return record, nil
}

func normalizeChangeKey(input ChangeKey) (ChangeKey, error) {
	provider, err := normalizeCandidateProvider(input.Provider)
	if err != nil {
		return ChangeKey{}, err
	}
	repository, err := normalizeCandidateRepository(input.Repository)
	if err != nil {
		return ChangeKey{}, err
	}
	if input.Number < 1 {
		return ChangeKey{}, NewError(CodeValidation, errors.New("change number must be positive"))
	}
	return ChangeKey{Provider: provider, Repository: repository, Number: input.Number}, nil
}

func validateLandingPlanInput(input LandingPlanInput) (ContextObject, []Entity, []ContextSnapshotProvenance, CandidateContextDelta, error) {
	if input.Kind != ContextPlanPreview && input.Kind != ContextPlanFinal {
		return ContextObject{}, nil, nil, CandidateContextDelta{}, NewError(CodeValidation, errors.New("context plan kind is invalid"))
	}
	if input.CreatedAt.IsZero() || strings.TrimSpace(input.Fingerprint.RepositoryID) == "" || strings.TrimSpace(input.Fingerprint.TargetRef) == "" || input.Fingerprint.BaseContextVersion < 0 || !validCandidateSHA(input.Fingerprint.BaseSourceSHA) || !validCandidateSHA(input.Fingerprint.HeadSourceSHA) {
		return ContextObject{}, nil, nil, CandidateContextDelta{}, NewError(CodeValidation, errors.New("context plan fingerprint is incomplete or invalid"))
	}
	change, err := normalizeChangeKey(input.Fingerprint.Change)
	if err != nil || change != input.Fingerprint.Change {
		return ContextObject{}, nil, nil, CandidateContextDelta{}, NewError(CodeValidation, errors.New("context plan change key is not canonical"))
	}
	if !IsContextSnapshotSchema(input.Canonical.SchemaVersion) || input.Canonical.RepositoryID != input.Fingerprint.RepositoryID || input.Canonical.RefName != input.Fingerprint.TargetRef || input.Canonical.SourceSHA != input.Fingerprint.BaseSourceSHA {
		return ContextObject{}, nil, nil, CandidateContextDelta{}, NewError(CodeValidation, errors.New("canonical context does not match the plan fingerprint"))
	}
	if input.Fingerprint.BaseContextCommitID != "" {
		normalized, err := NormalizeContextCommitID(input.Fingerprint.BaseContextCommitID)
		if err != nil || normalized != input.Fingerprint.BaseContextCommitID {
			return ContextObject{}, nil, nil, CandidateContextDelta{}, NewError(CodeValidation, errors.New("context plan base context ID is invalid"))
		}
	}
	entities, err := normalizeLandingEntities(input.TargetEntities, input.Fingerprint.HeadSourceSHA)
	if err != nil || DigestSourceEvidence(entities) != input.Fingerprint.SourceEvidenceDigest {
		return ContextObject{}, nil, nil, CandidateContextDelta{}, NewError(CodeValidation, errors.New("target source evidence does not match the plan fingerprint"))
	}
	provenance, provenanceErr := normalizeLandingProvenance(input.TargetProvenance, input.Fingerprint.HeadSourceSHA)
	if input.CoverageComplete && provenanceErr != nil {
		return ContextObject{}, nil, nil, CandidateContextDelta{}, provenanceErr
	}
	if provenanceErr == nil && DigestProvenance(provenance) != input.Fingerprint.ProvenanceDigest {
		return ContextObject{}, nil, nil, CandidateContextDelta{}, NewError(CodeValidation, errors.New("target provenance does not match the plan fingerprint"))
	}
	candidate := CandidateContextDelta{}
	if input.Candidate.SchemaVersion == 0 && len(input.Candidate.Records) == 0 {
		if input.Fingerprint.CandidateDigest != "" {
			return ContextObject{}, nil, nil, CandidateContextDelta{}, NewError(CodeValidation, errors.New("context plan candidate digest has no candidate delta"))
		}
	} else {
		candidate, err = NormalizeCandidateContextDelta(input.Candidate)
		if err != nil {
			return ContextObject{}, nil, nil, CandidateContextDelta{}, err
		}
		digest, err := CandidateContextDigest(candidate)
		sourceMismatch := input.Kind == ContextPlanPreview && (candidate.BaseSourceSHA != input.Fingerprint.BaseSourceSHA || candidate.HeadSourceSHA != input.Fingerprint.HeadSourceSHA)
		if err != nil || digest != input.Fingerprint.CandidateDigest || candidate.Change != input.Fingerprint.Change || sourceMismatch || candidate.BaseContextCommitID != input.Fingerprint.BaseContextCommitID {
			return ContextObject{}, nil, nil, CandidateContextDelta{}, NewError(CodeValidation, errors.New("candidate context delta does not match the plan fingerprint"))
		}
	}
	return input.Canonical, entities, provenance, candidate, nil
}

func normalizeLandingEntities(input []Entity, sourceSHA string) ([]Entity, error) {
	entities := append([]Entity(nil), input...)
	sort.Slice(entities, func(i, j int) bool { return entities[i].Key < entities[j].Key })
	previousKey := ""
	for _, entity := range entities {
		if entity.Language == "" || entity.Key == "" || entity.Path == "" || entity.SourceSHA != sourceSHA || !validCandidateSHA(entity.StructuralHash) || (previousKey != "" && entity.Key <= previousKey) {
			return nil, NewError(CodeValidation, errors.New("landing source evidence contains invalid or duplicate entities"))
		}
		previousKey = entity.Key
	}
	return entities, nil
}

func normalizeLandingProvenance(input []ContextSnapshotProvenance, sourceSHA string) ([]ContextSnapshotProvenance, error) {
	provenance := append([]ContextSnapshotProvenance(nil), input...)
	sort.Slice(provenance, func(i, j int) bool { return provenance[i].Language < provenance[j].Language })
	previousLanguage := ""
	for _, item := range provenance {
		if item.Language == "" || item.IndexerID == "" || item.IndexerVersion == "" || item.SourceSHA != sourceSHA || (previousLanguage != "" && item.Language <= previousLanguage) {
			return nil, NewError(CodeValidation, errors.New("landing provenance is invalid or incomplete"))
		}
		previousLanguage = item.Language
	}
	return provenance, nil
}

func applyCandidateRecord(record CandidateContextRecord, decisions map[string]LandingDecision, targetEntities []Entity, sourceSHA string, conflicts *[]LandingConflict) {
	existingDecision, found := decisions[record.NoteID]
	desired := noteFromCandidateRecord(record, targetEntities, sourceSHA)
	conflict := func(reason string) {
		canonicalRevision := ""
		if found {
			canonicalRevision = existingDecision.Note.RevisionID
		}
		*conflicts = append(*conflicts, LandingConflict{CandidateRecordID: record.ID, NoteID: record.NoteID, Reason: reason, BaseRevisionID: record.BaseRevisionID, CanonicalRevisionID: canonicalRevision})
	}
	switch record.Operation {
	case CandidateAdd:
		if found {
			if sameLandingNote(existingDecision.Note, desired) {
				existingDecision.CandidateRecordID = record.ID
				existingDecision.Operation = record.Operation
				existingDecision.NoOp = true
				decisions[record.NoteID] = existingDecision
				return
			}
			conflict("note_collision")
			return
		}
		decisions[record.NoteID] = landingDecision(record.ID, record.Operation, desired, false)
	case CandidateRevise:
		if !found {
			conflict("revision_conflict")
			return
		}
		if existingDecision.Note.RevisionID == record.RevisionID {
			if sameLandingNote(existingDecision.Note, desired) {
				existingDecision.CandidateRecordID = record.ID
				existingDecision.Operation = record.Operation
				existingDecision.NoOp = true
				decisions[record.NoteID] = existingDecision
				return
			}
			conflict("immutable_revision_conflict")
			return
		}
		if existingDecision.Note.RevisionID != record.BaseRevisionID {
			conflict("revision_conflict")
			return
		}
		decisions[record.NoteID] = landingDecision(record.ID, record.Operation, desired, false)
	case CandidateRebind:
		if !found || existingDecision.Note.RevisionID != record.BaseRevisionID {
			conflict("revision_conflict")
			return
		}
		if !candidateRecordPreservesRevision(record, existingDecision.Note) {
			conflict("immutable_revision_conflict")
			return
		}
		desired = existingDecision.Note
		desired.EntityKey = record.EntityKey
		desired = bindCandidateNote(desired, record.StructuralHash, targetEntities, sourceSHA)
		decisions[record.NoteID] = landingDecision(record.ID, record.Operation, desired, sameLandingNote(existingDecision.Note, desired))
	}
}

func noteFromCandidateRecord(record CandidateContextRecord, targetEntities []Entity, sourceSHA string) Note {
	note := Note{ID: record.NoteID, RevisionID: record.RevisionID, SupersedesRevisionID: record.SupersedesRevisionID, EntityKey: record.EntityKey, Kind: record.Kind, Body: record.Body, Author: record.Author, Origin: record.Origin, CreatedAt: record.CreatedAt, Topics: append([]string(nil), record.Topics...)}
	return bindCandidateNote(note, record.StructuralHash, targetEntities, sourceSHA)
}

func bindCandidateNote(note Note, structuralHash string, targetEntities []Entity, sourceSHA string) Note {
	note.Pending = false
	note.BindingSourceSHA = sourceSHA
	note.ReviewReason = ""
	for _, entity := range targetEntities {
		if entity.Key != note.EntityKey {
			continue
		}
		if entity.StructuralHash == structuralHash {
			note.BindingState = NoteBindingActive
			return note
		}
		note.BindingState = NoteBindingNeedsReview
		note.ReviewReason = "structural_change"
		return note
	}
	note.BindingState = NoteBindingHistorical
	note.ReviewReason = "entity_removed"
	return note
}

func candidateRecordPreservesRevision(record CandidateContextRecord, note Note) bool {
	return record.NoteID == note.ID && record.RevisionID == note.RevisionID && record.Kind == note.Kind && record.Body == note.Body && slices.Equal(record.Topics, note.Topics) && record.Author == note.Author && record.Origin == note.Origin && record.CreatedAt.Equal(note.CreatedAt)
}

func sameLandingNote(left, right Note) bool {
	left.Pending = false
	right.Pending = false
	return reflect.DeepEqual(left, right)
}

func landingDecision(recordID string, operation CandidateContextOperation, note Note, noOp bool) LandingDecision {
	note.Pending = false
	mapping := ContextRevisionMapping{EntityKey: note.EntityKey, NoteID: note.ID, RevisionID: note.RevisionID, BindingState: note.BindingState, BindingSourceSHA: note.BindingSourceSHA, ReviewReason: note.ReviewReason}
	return LandingDecision{CandidateRecordID: recordID, Operation: operation, Note: note, Mapping: mapping, NoOp: noOp}
}

func contextPlanID(plan ContextPlan) (string, error) {
	canonical := plan
	canonical.ID = ""
	canonical.CreatedAt = time.Time{}
	return canonicalDigest(canonical)
}

func canonicalDigest(value any) (string, error) {
	contents, err := json.Marshal(value)
	if err != nil {
		return "", NewError(CodeValidation, fmt.Errorf("serialize canonical landing content: %w", err))
	}
	digest := blake3.Sum256(contents)
	return fmt.Sprintf("%x", digest[:]), nil
}

func digestParts(parts ...string) string {
	digest := blake3.New()
	for _, part := range parts {
		_, _ = digest.Write([]byte(part))
		_, _ = digest.Write([]byte{0})
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}
