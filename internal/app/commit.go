package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/store"
	"github.com/zeebo/blake3"
)

func (s *Service) Commit(ctx context.Context, input CommitInput) (domain.ContextCommit, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	message := strings.TrimSpace(input.Message)
	if message == "" {
		return domain.ContextCommit{}, domain.NewError(domain.CodeValidation, errors.New("commit message must not be empty"))
	}
	author := strings.TrimSpace(input.Author)
	if author == "" {
		author = defaultAuthor(ctx, state)
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	snapshot, err := contextStore.CommitSnapshot(ctx, key)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if snapshot.WorkingSource != state.HeadSHA {
		return domain.ContextCommit{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("working source differs from Git HEAD; run update before commit"))
	}
	complete, err := contextStore.CoverageComplete(ctx, key)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if !complete {
		return domain.ContextCommit{}, domain.NewError(domain.CodeCoverageIncomplete, errors.New("language coverage is incomplete; install missing packs and run update before commit"))
	}
	status, err := contextStore.Status(ctx, key)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	provenance, err := contextSnapshotProvenance(status.Coverage, state.HeadSHA)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if len(snapshot.PendingNotes) == 0 {
		return domain.ContextCommit{}, domain.NewError(domain.CodeNothingToCommit, errors.New("no pending context changes"))
	}
	pendingNoteIDs := make([]string, 0, len(snapshot.PendingNotes))
	for _, note := range snapshot.PendingNotes {
		pendingNoteIDs = append(pendingNoteIDs, note.ID)
	}
	notes := mergeNotes(snapshot.CommittedNotes, snapshot.PendingNotes)
	for index := range notes {
		notes[index].Pending = false
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].ID < notes[j].ID })
	entities := append([]domain.Entity{}, snapshot.Entities...)
	sort.Slice(entities, func(i, j int) bool { return entities[i].Key < entities[j].Key })
	parentIDs, legacyParentID, schemaVersion, err := contextSnapshotParents(contextStore, snapshot.ParentID, key)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	createdAt := time.Now().UTC()
	object := domain.ContextObject{
		SchemaVersion:    schemaVersion,
		RepositoryID:     key.RepositoryID,
		RefName:          key.RefName,
		ParentIDs:        parentIDs,
		LegacyParentID:   legacyParentID,
		SourceSHA:        state.HeadSHA,
		Message:          message,
		Author:           author,
		CreatedAt:        createdAt,
		Provenance:       provenance,
		Entities:         entities,
		Notes:            notes,
		RevisionMappings: contextRevisionMappings(notes),
	}
	contents, err := json.Marshal(object)
	if err != nil {
		return domain.ContextCommit{}, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("serialize context object: %w", err))
	}
	digest := blake3.Sum256(contents)
	identifier := fmt.Sprintf("%x", digest[:])
	if err := s.requireCommitSourceState(ctx, state); err != nil {
		return domain.ContextCommit{}, err
	}
	if err := contextStore.WriteObject(identifier, contents); err != nil {
		return domain.ContextCommit{}, err
	}
	if err := s.requireCommitSourceState(ctx, state); err != nil {
		return domain.ContextCommit{}, err
	}
	commit := domain.ContextCommit{ID: identifier, ParentID: snapshot.ParentID, RefName: key.RefName, SourceSHA: state.HeadSHA, Message: message, Author: author, CreatedAt: createdAt}
	if err := contextStore.FinalizeCommit(ctx, store.FinalizeInput{Key: key, ExpectedParent: snapshot.ParentID, PendingNoteIDs: pendingNoteIDs, Commit: commit, Notes: notes}); err != nil {
		return domain.ContextCommit{}, err
	}
	return commit, nil
}

func mergeNotes(committed, pending []domain.Note) []domain.Note {
	merged := make(map[string]domain.Note, len(committed)+len(pending))
	for _, note := range committed {
		merged[note.ID] = note
	}
	for _, note := range pending {
		merged[note.ID] = note
	}
	notes := make([]domain.Note, 0, len(merged))
	for _, note := range merged {
		notes = append(notes, note)
	}
	return notes
}

func contextSnapshotProvenance(coverage []domain.Coverage, sourceSHA string) ([]domain.ContextSnapshotProvenance, error) {
	provenance := make([]domain.ContextSnapshotProvenance, 0, len(coverage))
	for _, item := range coverage {
		if item.Language == "" || item.State != domain.CoverageIndexed || item.IndexerID == "" || item.IndexerVersion == "" || item.SourceSHA != sourceSHA {
			return nil, domain.NewError(domain.CodeValidation, errors.New("complete coverage must provide snapshot provenance"))
		}
		provenance = append(provenance, domain.ContextSnapshotProvenance{Language: item.Language, IndexerID: item.IndexerID, IndexerVersion: item.IndexerVersion, SourceSHA: item.SourceSHA})
	}
	sort.Slice(provenance, func(i, j int) bool { return provenance[i].Language < provenance[j].Language })
	for index := 1; index < len(provenance); index++ {
		if provenance[index-1].Language == provenance[index].Language {
			return nil, domain.NewError(domain.CodeValidation, errors.New("complete coverage contains duplicate snapshot provenance languages"))
		}
	}
	return provenance, nil
}

func sameContextSnapshotProvenance(left, right []domain.ContextSnapshotProvenance) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func contextSnapshotParents(contextStore *store.Store, parentID string, key domain.WorkingSetKey) ([]string, string, int, error) {
	if parentID == "" {
		return nil, "", 3, nil
	}
	parent, err := contextStore.ReadContextObject(parentID, key.RepositoryID, key.RefName)
	if err != nil {
		return nil, "", 0, err
	}
	if !domain.IsContextSnapshotSchema(parent.SchemaVersion) {
		return nil, parentID, 3, nil
	}
	return []string{parentID}, "", contextSnapshotWriteVersion(parent), nil
}

func contextSnapshotWriteVersion(parents ...domain.ContextObject) int {
	for _, parent := range parents {
		if parent.SchemaVersion == 4 {
			return 4
		}
	}
	return 3
}

func contextRevisionMappings(notes []domain.Note) []domain.ContextRevisionMapping {
	mappings := make([]domain.ContextRevisionMapping, 0, len(notes))
	for _, note := range notes {
		mappings = append(mappings, domain.ContextRevisionMapping{EntityKey: note.EntityKey, NoteID: note.ID, RevisionID: note.RevisionID, BindingState: note.BindingState, BindingSourceSHA: note.BindingSourceSHA, ReviewReason: note.ReviewReason})
	}
	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].EntityKey != mappings[j].EntityKey {
			return mappings[i].EntityKey < mappings[j].EntityKey
		}
		if mappings[i].NoteID != mappings[j].NoteID {
			return mappings[i].NoteID < mappings[j].NoteID
		}
		return mappings[i].RevisionID < mappings[j].RevisionID
	})
	return mappings
}

func (s *Service) Log(ctx context.Context, limit int) ([]domain.ContextCommit, error) {
	_, key, err := s.mutableKey(ctx)
	if err != nil {
		return nil, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	return contextStore.Log(ctx, key, limit)
}
