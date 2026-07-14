package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/store"
)

func (s *Service) Update(ctx context.Context) (UpdateResult, error) {
	return s.UpdateWithOptions(ctx, false)
}

func (s *Service) UpdateWithOptions(ctx context.Context, requireComplete bool) (UpdateResult, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return UpdateResult{}, err
	}
	if err := state.RequireCleanWorktree(ctx); err != nil {
		return UpdateResult{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return UpdateResult{}, err
	}
	projections, err := s.indexer.Index(ctx, state.Root, state.HeadSHA)
	if err != nil {
		return UpdateResult{}, domain.NewError(domain.CodeValidation, err)
	}
	currentState, err := s.state(ctx)
	if err != nil {
		return UpdateResult{}, err
	}
	if !sameSourceState(state, currentState) {
		return UpdateResult{}, domain.NewError(domain.CodeRepositoryState, errors.New("Git source state changed while indexing; retry update"))
	}
	if err := currentState.RequireCleanWorktree(ctx); err != nil {
		return UpdateResult{}, err
	}
	if err := contextStore.ApplyIndexUpdate(ctx, key, projections); err != nil {
		return UpdateResult{}, err
	}
	status, err := contextStore.Status(ctx, key)
	if err != nil {
		return UpdateResult{}, err
	}
	result := UpdateResult{IndexedEntities: status.EntityCount, SourceSHA: state.HeadSHA, CoverageComplete: status.CoverageComplete, Coverage: status.Coverage}
	for _, coverage := range status.Coverage {
		if coverage.State == domain.CoverageFailed {
			return result, domain.NewError(domain.CodeValidation, fmt.Errorf("%s indexer failed: %s", coverage.Language, coverage.Detail))
		}
	}
	if requireComplete && !result.CoverageComplete {
		return result, domain.NewError(domain.CodeCoverageIncomplete, errors.New("language coverage is incomplete; install missing packs and run update"))
	}
	return result, nil
}

func (s *Service) Rebuild(ctx context.Context, commitID string) (RebuildResult, error) {
	commitID, err := domain.NormalizeContextCommitID(commitID)
	if err != nil {
		return RebuildResult{}, err
	}
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return RebuildResult{}, err
	}
	if err := state.RequireCleanWorktree(ctx); err != nil {
		return RebuildResult{}, err
	}
	selectedSnapshot, err := store.ReadContextObject(s.layout, commitID, key.RepositoryID, key.RefName)
	if err != nil {
		return RebuildResult{}, err
	}
	if domain.IsContextSnapshotSchema(selectedSnapshot.SchemaVersion) && selectedSnapshot.SourceSHA != state.HeadSHA {
		return RebuildResult{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("context snapshot source does not match the current Git HEAD"))
	}
	projections, err := s.indexer.Index(ctx, state.Root, state.HeadSHA)
	if err != nil {
		return RebuildResult{}, domain.NewError(domain.CodeValidation, err)
	}
	for _, projection := range projections {
		if projection.Coverage.State == domain.CoverageFailed {
			return RebuildResult{}, domain.NewError(domain.CodeValidation, fmt.Errorf("%s indexer failed: %s", projection.Coverage.Language, projection.Coverage.Detail))
		}
	}
	currentState, err := s.state(ctx)
	if err != nil {
		return RebuildResult{}, err
	}
	if !sameSourceState(state, currentState) {
		return RebuildResult{}, domain.NewError(domain.CodeRepositoryState, errors.New("Git source state changed while rebuilding; retry rebuild"))
	}
	if err := currentState.RequireCleanWorktree(ctx); err != nil {
		return RebuildResult{}, err
	}
	if domain.IsContextSnapshotSchema(selectedSnapshot.SchemaVersion) {
		provenance, err := rebuildSnapshotProvenance(projections, state.HeadSHA)
		if err != nil {
			return RebuildResult{}, err
		}
		if !sameContextSnapshotProvenance(selectedSnapshot.Provenance, provenance) {
			return RebuildResult{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("context snapshot provenance does not match the current indexers"))
		}
	}
	contextStore, err := s.openStore(ctx, true)
	if err != nil {
		return RebuildResult{}, err
	}
	tipID, restored, err := contextStore.Rebuild(ctx, store.RebuildInput{Key: key, CommitID: commitID, Projections: projections})
	if err != nil {
		return RebuildResult{}, err
	}
	status, err := contextStore.Status(ctx, key)
	if err != nil {
		return RebuildResult{}, err
	}
	return RebuildResult{ContextCommitID: tipID, RestoredCommits: restored, IndexedEntities: status.EntityCount, SourceSHA: status.SourceSHA, CoverageComplete: status.CoverageComplete, Coverage: status.Coverage}, nil
}

func rebuildSnapshotProvenance(projections []domain.LanguageProjection, sourceSHA string) ([]domain.ContextSnapshotProvenance, error) {
	coverage := make([]domain.Coverage, 0, len(projections))
	for _, projection := range projections {
		if projection.Coverage.State != domain.CoverageIndexed {
			return nil, domain.NewError(domain.CodeCoverageIncomplete, errors.New("snapshot provenance requires complete indexed language coverage"))
		}
		coverage = append(coverage, projection.Coverage)
	}
	return contextSnapshotProvenance(coverage, sourceSHA)
}
