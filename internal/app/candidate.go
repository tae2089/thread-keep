package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
)

func (s *Service) ImportCandidate(ctx context.Context, path string) (CandidateImportResult, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return CandidateImportResult{}, domain.NewError(domain.CodeValidation, errors.New("candidate envelope path must not be empty"))
	}
	file, err := os.Open(path)
	if err != nil {
		return CandidateImportResult{}, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("open candidate envelope: %w", err))
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, domain.MaxCandidateEnvelopeBytes+1))
	if err != nil {
		return CandidateImportResult{}, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("read candidate envelope: %w", err))
	}
	candidate, notes, err := domain.DecodeCandidateEnvelope(contents)
	if err != nil {
		return CandidateImportResult{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return CandidateImportResult{}, err
	}
	imported, err := contextStore.ImportCandidate(ctx, candidate, notes)
	if err != nil {
		return CandidateImportResult{}, err
	}
	return CandidateImportResult{Candidate: candidate, DraftNotes: len(notes), Imported: imported}, nil
}

func (s *Service) PromoteCandidate(ctx context.Context, candidateID string) (domain.CandidatePromotionResult, error) {
	_, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.CandidatePromotionResult{}, err
	}
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return domain.CandidatePromotionResult{}, domain.NewError(domain.CodeValidation, errors.New("candidate ID must not be empty"))
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.CandidatePromotionResult{}, err
	}
	return contextStore.PromoteCandidate(ctx, key, candidateID)
}

func (s *Service) Candidates(ctx context.Context) ([]domain.Candidate, error) {
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	return contextStore.Candidates(ctx)
}

func (s *Service) Candidate(ctx context.Context, candidateID string) (CandidateResult, error) {
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return CandidateResult{}, domain.NewError(domain.CodeValidation, errors.New("candidate ID must not be empty"))
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return CandidateResult{}, err
	}
	candidate, notes, err := contextStore.Candidate(ctx, candidateID)
	if err != nil {
		return CandidateResult{}, err
	}
	return CandidateResult{Candidate: candidate, Notes: notes}, nil
}

func (s *Service) PublishCandidate(ctx context.Context, remoteName, changeValue string) (CandidatePublishResult, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	if err := state.RequireCleanWorktree(ctx); err != nil {
		return CandidatePublishResult{}, err
	}
	change, err := domain.ParseChangeKey(changeValue)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	pending, err := contextStore.PendingNotes(ctx, key)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	if len(pending) != 0 {
		return CandidatePublishResult{}, domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit pending context changes before publishing a candidate"))
	}
	localRef, err := contextStore.ContextRef(ctx, key)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	if localRef.CommitID == "" || localRef.SourceSHA != state.HeadSHA {
		return CandidatePublishResult{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("local context tip must match the current Git HEAD"))
	}
	configured, err := contextStore.Remote(ctx, remoteName)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	transport, err := remote.Dial(configured.Path)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	candidateTransport, ok := transport.(remote.CandidateTransport)
	if !ok {
		return CandidatePublishResult{}, domain.NewError(domain.CodeValidation, errors.New("configured remote does not support PR candidate publication"))
	}
	metadata, err := candidateTransport.CandidatePublicationMetadata(ctx, change)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	if metadata.HeadSourceSHA != state.HeadSHA {
		return CandidatePublishResult{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("remote PR head does not match the current Git HEAD"))
	}
	ancestor, err := contextStore.IsAncestor(localRef.CommitID, metadata.BaseContextCommitID, key.RepositoryID, key.RefName)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	if !ancestor {
		return CandidatePublishResult{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("remote PR base context is not in the local context ancestry"))
	}
	base, err := contextStore.ReadContextObject(metadata.BaseContextCommitID, key.RepositoryID, key.RefName)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	branch, err := contextStore.ReadContextObject(localRef.CommitID, key.RepositoryID, key.RefName)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	if base.SourceSHA != metadata.BaseSourceSHA || branch.SourceSHA != metadata.HeadSourceSHA {
		return CandidatePublishResult{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("candidate source metadata does not match local context ancestry"))
	}
	delta, err := buildCandidateContextDelta(change, metadata, base, branch)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	digest, err := domain.CandidateContextDigest(delta)
	if err != nil {
		return CandidatePublishResult{}, err
	}
	result, err := candidateTransport.PublishCandidate(ctx, remote.CandidatePublicationRequest{Delta: delta, Digest: digest})
	if err != nil {
		return CandidatePublishResult{}, err
	}
	return CandidatePublishResult{Digest: result.Digest, Records: len(delta.Records), Published: result.Published}, nil
}

func buildCandidateContextDelta(change domain.ChangeKey, metadata remote.CandidatePublicationMetadata, base, branch domain.ContextObject) (domain.CandidateContextDelta, error) {
	baseNotes := make(map[string]domain.Note, len(base.Notes))
	branchNotes := make(map[string]domain.Note, len(branch.Notes))
	branchEntities := make(map[string]domain.Entity, len(branch.Entities))
	baseEntities := make(map[string]domain.Entity, len(base.Entities))
	for _, note := range base.Notes {
		baseNotes[note.ID] = note
	}
	for _, note := range branch.Notes {
		branchNotes[note.ID] = note
	}
	for _, entity := range base.Entities {
		baseEntities[entity.Key] = entity
	}
	for _, entity := range branch.Entities {
		branchEntities[entity.Key] = entity
	}
	for noteID := range baseNotes {
		if _, found := branchNotes[noteID]; !found {
			return domain.CandidateContextDelta{}, domain.NewError(domain.CodeValidation, errors.New("candidate context note deletion is not supported"))
		}
	}
	records := make([]domain.CandidateContextRecord, 0)
	for noteID, note := range branchNotes {
		previous, found := baseNotes[noteID]
		operation := domain.CandidateAdd
		baseRevisionID := ""
		supersedesRevisionID := ""
		if found {
			switch {
			case note.RevisionID != previous.RevisionID:
				operation = domain.CandidateRevise
				baseRevisionID = previous.RevisionID
				supersedesRevisionID = note.SupersedesRevisionID
			case note.EntityKey != previous.EntityKey:
				if !sameImmutableCandidateRevision(previous, note) {
					return domain.CandidateContextDelta{}, domain.NewError(domain.CodeValidation, errors.New("candidate rebind changes immutable revision content"))
				}
				operation = domain.CandidateRebind
				baseRevisionID = previous.RevisionID
			default:
				if !sameImmutableCandidateRevision(previous, note) {
					return domain.CandidateContextDelta{}, domain.NewError(domain.CodeValidation, errors.New("candidate changes immutable content without a new revision"))
				}
				continue
			}
		}
		entity, found := branchEntities[note.EntityKey]
		if !found {
			entity, found = baseEntities[note.EntityKey]
		}
		if !found {
			return domain.CandidateContextDelta{}, domain.NewError(domain.CodeValidation, errors.New("candidate note has no structural entity evidence"))
		}
		records = append(records, domain.CandidateContextRecord{Operation: operation, NoteID: note.ID, RevisionID: note.RevisionID, BaseRevisionID: baseRevisionID, SupersedesRevisionID: supersedesRevisionID, EntityKey: note.EntityKey, StructuralHash: entity.StructuralHash, Kind: note.Kind, Body: note.Body, Topics: note.Topics, Author: note.Author, Origin: note.Origin, CreatedAt: note.CreatedAt})
	}
	return domain.NormalizeCandidateContextDelta(domain.CandidateContextDelta{SchemaVersion: 2, Change: change, BaseSourceSHA: metadata.BaseSourceSHA, HeadSourceSHA: metadata.HeadSourceSHA, BaseContextCommitID: metadata.BaseContextCommitID, Records: records})
}

func sameImmutableCandidateRevision(left, right domain.Note) bool {
	return left.ID == right.ID && left.RevisionID == right.RevisionID && left.SupersedesRevisionID == right.SupersedesRevisionID && left.Kind == right.Kind && left.Body == right.Body && slices.Equal(left.Topics, right.Topics) && left.Author == right.Author && left.Origin == right.Origin && left.CreatedAt.Equal(right.CreatedAt)
}
