package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

func (s *Service) AddNote(ctx context.Context, input AddNoteInput) (domain.Note, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.Note{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.Note{}, err
	}
	entityKey := strings.TrimSpace(input.EntityKey)
	if entityKey == "" {
		return domain.Note{}, domain.NewError(domain.CodeValidation, errors.New("entity key must not be empty"))
	}
	kind := domain.NoteKind(input.Kind)
	if !domain.ValidNoteKind(kind) {
		return domain.Note{}, domain.NewError(domain.CodeValidation, fmt.Errorf("unsupported note kind %q", input.Kind))
	}
	body := strings.TrimSpace(input.Body)
	if body == "" {
		return domain.Note{}, domain.NewError(domain.CodeValidation, errors.New("note body must not be empty"))
	}
	if len(body) > 64*1024 {
		return domain.Note{}, domain.NewError(domain.CodeValidation, errors.New("note body exceeds 64 KiB"))
	}
	author := strings.TrimSpace(input.Author)
	if author == "" {
		author = defaultAuthor(ctx, state)
	}
	origin := strings.TrimSpace(input.Origin)
	if origin == "" {
		origin = "human"
	}
	topics, err := domain.NormalizeNoteTopics(input.Topics)
	if err != nil {
		return domain.Note{}, err
	}
	return contextStore.AddPendingNote(ctx, key, domain.Note{EntityKey: entityKey, Kind: kind, Body: body, Author: author, Origin: origin, Topics: topics})
}

func (s *Service) ReviseNote(ctx context.Context, input ReviseNoteInput) (domain.Note, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.Note{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.Note{}, err
	}
	noteID := strings.TrimSpace(input.NoteID)
	if noteID == "" {
		return domain.Note{}, domain.NewError(domain.CodeValidation, errors.New("note ID must not be empty"))
	}
	body := strings.TrimSpace(input.Body)
	if body == "" {
		return domain.Note{}, domain.NewError(domain.CodeValidation, errors.New("note body must not be empty"))
	}
	if len(body) > 64*1024 {
		return domain.Note{}, domain.NewError(domain.CodeValidation, errors.New("note body exceeds 64 KiB"))
	}
	author := strings.TrimSpace(input.Author)
	if author == "" {
		author = defaultAuthor(ctx, state)
	}
	origin := strings.TrimSpace(input.Origin)
	if origin == "" {
		origin = "human"
	}
	var topics []string
	if input.Topics != nil {
		topics, err = domain.NormalizeNoteTopics(input.Topics)
		if err != nil {
			return domain.Note{}, err
		}
	}
	return contextStore.ReviseNote(ctx, key, noteID, body, author, origin, topics)
}

func (s *Service) ReviewNote(ctx context.Context, input ReviewNoteInput) (domain.Note, error) {
	_, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.Note{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.Note{}, err
	}
	noteID := strings.TrimSpace(input.NoteID)
	if noteID == "" {
		return domain.Note{}, domain.NewError(domain.CodeValidation, errors.New("note ID must not be empty"))
	}
	entityKey := strings.TrimSpace(input.EntityKey)
	if entityKey == "" {
		return domain.Note{}, domain.NewError(domain.CodeValidation, errors.New("entity key must not be empty"))
	}
	return contextStore.ReviewNote(ctx, key, noteID, entityKey)
}
