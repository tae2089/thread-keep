package domain

import (
	"sort"
	"strings"
)

type BindingReconciler struct {
	currentByKey    map[string]Entity
	currentEntities []Entity
	sourceSHA       string
}

type CandidatePromotionMapping struct {
	CandidateRecordID string `json:"candidate_record_id"`
	NoteID            string `json:"note_id"`
	RevisionID        string `json:"revision_id"`
}

type LandingReceipt struct {
	ID                  string                      `json:"id"`
	Provider            string                      `json:"provider"`
	ForgeRepository     string                      `json:"forge_repository"`
	ChangeNumber        int                         `json:"change_number"`
	ContextRepositoryID string                      `json:"context_repository_id"`
	TargetRef           string                      `json:"target_ref"`
	CandidateDigest     string                      `json:"candidate_digest,omitempty"`
	FinalPlanID         string                      `json:"final_plan_id"`
	SourceMergeSHA      string                      `json:"source_merge_sha"`
	BaseContextCommitID string                      `json:"base_context_commit_id,omitempty"`
	Resolver            string                      `json:"resolver"`
	CandidateMappings   []CandidatePromotionMapping `json:"candidate_mappings,omitempty"`
}

func NewBindingReconciler(currentEntities []Entity, sourceSHA string) BindingReconciler {
	currentByKey := make(map[string]Entity, len(currentEntities))
	for _, entity := range currentEntities {
		currentByKey[entity.Key] = entity
	}
	return BindingReconciler{
		currentByKey:    currentByKey,
		currentEntities: append([]Entity(nil), currentEntities...),
		sourceSHA:       sourceSHA,
	}
}

func (r BindingReconciler) Reconcile(note Note, previous Entity) (Note, bool) {
	updated := note
	updated.Pending = true
	updated.BindingSourceSHA = r.sourceSHA
	updated.ReviewReason = ""
	if previous.Key == "" {
		updated.BindingState = NoteBindingNeedsReview
		updated.ReviewReason = "unknown_lineage"
		return updated, true
	}
	if current, found := r.currentByKey[note.EntityKey]; found {
		if current.StructuralHash == previous.StructuralHash {
			return updated, updated.BindingSourceSHA != note.BindingSourceSHA
		}
		updated.BindingState = NoteBindingNeedsReview
		updated.ReviewReason = "structural_change"
		return updated, true
	}
	var candidates []Entity
	for _, entity := range r.currentEntities {
		if entity.Language == previous.Language && entity.Kind == previous.Kind && entity.StructuralHash == previous.StructuralHash {
			candidates = append(candidates, entity)
		}
	}
	if len(candidates) == 1 {
		updated.EntityKey = candidates[0].Key
		return updated, true
	}
	if len(candidates) > 1 {
		updated.BindingState = NoteBindingNeedsReview
		updated.ReviewReason = "ambiguous_lineage"
		return updated, true
	}
	updated.BindingState = NoteBindingHistorical
	updated.ReviewReason = "entity_removed"
	return updated, true
}

func IsContextSnapshotSchema(version int) bool {
	return version == 3 || version == 4
}

func ClassifyEntityChanges(base, current []Entity) []EntityChange {
	baseByKey := make(map[string]Entity, len(base))
	currentByKey := make(map[string]Entity, len(current))
	unmatchedBase := make(map[string]Entity)
	unmatchedCurrent := make(map[string]Entity)
	var changes []EntityChange
	for _, entity := range base {
		baseByKey[entity.Key] = entity
	}
	for _, entity := range current {
		currentByKey[entity.Key] = entity
	}
	for key, previous := range baseByKey {
		next, found := currentByKey[key]
		if !found {
			unmatchedBase[key] = previous
			continue
		}
		if previous.StructuralHash != next.StructuralHash {
			changes = append(changes, EntityChange{Kind: ChangeModified, Base: previous, Target: next})
		}
	}
	for key, next := range currentByKey {
		if _, found := baseByKey[key]; !found {
			unmatchedCurrent[key] = next
		}
	}
	baseGroups := landingEntityGroups(unmatchedBase)
	currentGroups := landingEntityGroups(unmatchedCurrent)
	for signature, previous := range baseGroups {
		next := currentGroups[signature]
		if len(previous) != 1 || len(next) != 1 {
			continue
		}
		changes = append(changes, EntityChange{Kind: ChangeMoved, Base: previous[0], Target: next[0]})
		delete(unmatchedBase, previous[0].Key)
		delete(unmatchedCurrent, next[0].Key)
	}
	for _, entity := range unmatchedCurrent {
		changes = append(changes, EntityChange{Kind: ChangeAdded, Target: entity})
	}
	for _, entity := range unmatchedBase {
		changes = append(changes, EntityChange{Kind: ChangeRemoved, Base: entity})
	}
	sort.Slice(changes, func(left, right int) bool {
		leftKey := landingChangeKey(changes[left])
		rightKey := landingChangeKey(changes[right])
		if leftKey != rightKey {
			return leftKey < rightKey
		}
		return changes[left].Kind < changes[right].Kind
	})
	return changes
}

func landingEntityGroups(entities map[string]Entity) map[string][]Entity {
	groups := make(map[string][]Entity)
	for _, entity := range entities {
		signature := strings.Join([]string{entity.Language, string(entity.Kind), entity.StructuralHash}, "\x00")
		groups[signature] = append(groups[signature], entity)
	}
	return groups
}

func landingChangeKey(change EntityChange) string {
	if change.Target.Key != "" {
		return change.Target.Key
	}
	return change.Base.Key
}
