package app

import (
	"context"
	"errors"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

func (s *Service) Status(ctx context.Context) (domain.Status, error) {
	state, err := s.state(ctx)
	if err != nil {
		return domain.Status{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.Status{}, err
	}
	return contextStore.Status(ctx, keyForState(state))
}

func (s *Service) Search(ctx context.Context, query string) ([]domain.SearchHit, error) {
	_, key, err := s.mutableKey(ctx)
	if err != nil {
		return nil, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	return contextStore.Search(ctx, key, query)
}

func (s *Service) RelatedContext(ctx context.Context, entityKey string, limit int) ([]domain.RelatedEntity, error) {
	_, key, err := s.mutableKey(ctx)
	if err != nil {
		return nil, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	return contextStore.Related(ctx, key, strings.TrimSpace(entityKey), limit)
}

func (s *Service) Context(ctx context.Context, entityKey string) (ContextResult, error) {
	_, key, err := s.mutableKey(ctx)
	if err != nil {
		return ContextResult{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return ContextResult{}, err
	}
	entity, notes, err := contextStore.Context(ctx, key, entityKey)
	if err != nil {
		return ContextResult{}, err
	}
	return ContextResult{Entity: entity, Notes: notes}, nil
}

func (s *Service) AssembleContext(ctx context.Context, query domain.ContextQuery) (domain.ContextBundle, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	bundle, err := contextStore.AssembleContext(ctx, key, query)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	if query.Anchor.Kind == domain.AnchorChange {
		ancestor, err := state.IsAncestor(ctx, bundle.Source.BaseSourceSHA, state.HeadSHA)
		if err != nil {
			return domain.ContextBundle{}, err
		}
		if !ancestor {
			return domain.ContextBundle{}, domain.NewError(domain.CodeValidation, errors.New("base context source must be an ancestor of the current Git HEAD"))
		}
	}
	current, err := s.state(ctx)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	if !sameSourceState(state, current) {
		return domain.ContextBundle{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("Git source state changed while assembling context; retry"))
	}
	dirty, err := current.HasUncommittedChanges(ctx)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	if dirty {
		bundle.Diagnostics = append(bundle.Diagnostics, domain.RetrievalDiagnostic{Code: "uncommitted_changes_ignored", Detail: "context describes the indexed committed HEAD"})
	}
	return bundle, nil
}

func (s *Service) Diff(ctx context.Context) ([]domain.Note, error) {
	_, key, err := s.mutableKey(ctx)
	if err != nil {
		return nil, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	return contextStore.PendingNotes(ctx, key)
}
