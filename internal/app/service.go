package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/gitrepo"
	"github.com/tae2089/thread-keep/internal/indexing"
	"github.com/tae2089/thread-keep/internal/store"
)

type Service struct {
	workingDirectory string
	commonDir        string
	layout           store.Layout
	store            *store.Store
	indexer          indexCoordinator
	discover         stateDiscoverer
}

type indexCoordinator interface {
	Index(context.Context, string, string) ([]domain.LanguageProjection, error)
}

type stateDiscoverer func(context.Context, string) (gitrepo.State, error)

type UpdateResult struct {
	IndexedEntities  int               `json:"indexed_entities"`
	SourceSHA        string            `json:"source_sha"`
	CoverageComplete bool              `json:"coverage_complete"`
	Coverage         []domain.Coverage `json:"coverage"`
}

type AddNoteInput struct {
	EntityKey string
	Kind      string
	Body      string
	Author    string
	Origin    string
	Topics    []string
}

type ReviseNoteInput struct {
	NoteID string
	Body   string
	Author string
	Origin string
	Topics []string
}

type ReviewNoteInput struct {
	NoteID    string
	EntityKey string
}

type ContextResult struct {
	Entity domain.Entity `json:"entity"`
	Notes  []domain.Note `json:"notes"`
}

type CommitInput struct {
	Message string
	Author  string
}

type RebuildResult struct {
	ContextCommitID  string            `json:"context_commit_id"`
	RestoredCommits  int               `json:"restored_commits"`
	IndexedEntities  int               `json:"indexed_entities"`
	SourceSHA        string            `json:"source_sha"`
	CoverageComplete bool              `json:"coverage_complete"`
	Coverage         []domain.Coverage `json:"coverage"`
}

type fetchedRemote struct {
	Local  domain.ContextRef
	Remote domain.ContextRef
	Count  int
}

type CandidateImportResult struct {
	Candidate  domain.Candidate `json:"candidate"`
	DraftNotes int              `json:"draft_notes"`
	Imported   bool             `json:"imported"`
}

type CandidateResult struct {
	Candidate domain.Candidate       `json:"candidate"`
	Notes     []domain.CandidateNote `json:"notes"`
}

type CandidatePublishResult struct {
	Digest    string `json:"digest"`
	Records   int    `json:"records"`
	Published bool   `json:"published"`
}

type MergeStartInput struct {
	LocalSnapshotID  string
	RemoteSnapshotID string
	Message          string
	Author           string
}

type MergeResolveInput struct {
	SessionID  string
	ConflictID string
	Use        string
	EntityKey  string
	Kind       string
	Body       string
	Author     string
	Origin     string
}

func Open(ctx context.Context, workingDirectory string) (*Service, error) {
	state, err := gitrepo.Discover(ctx, workingDirectory)
	if err != nil {
		return nil, err
	}
	return &Service{
		workingDirectory: state.Root,
		commonDir:        state.CommonDir,
		layout:           store.NewLayout(state.CommonDir),
		indexer:          indexing.NewCoordinator(),
		discover:         gitrepo.Discover,
	}, nil
}

func (s *Service) Close() error {
	if s.store == nil {
		return nil
	}
	return s.store.Close()
}

func (s *Service) Init(ctx context.Context) error {
	contextStore, err := s.openStore(ctx, true)
	if err != nil {
		return err
	}
	return contextStore.Initialize(ctx)
}

func (s *Service) Indexers(ctx context.Context) ([]domain.IndexerStatus, error) {
	statuses, err := indexing.List(ctx, s.workingDirectory)
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("inspect installed indexers: %w", err))
	}
	return statuses, nil
}

func (s *Service) state(ctx context.Context) (gitrepo.State, error) {
	state, err := s.discover(ctx, s.workingDirectory)
	if err != nil {
		return gitrepo.State{}, err
	}
	if filepath.Clean(state.CommonDir) != filepath.Clean(s.commonDir) {
		return gitrepo.State{}, domain.NewError(domain.CodeRepositoryState, errors.New("Git common directory changed while thread-keep was running"))
	}
	return state, nil
}

func (s *Service) mutableKey(ctx context.Context) (gitrepo.State, domain.WorkingSetKey, error) {
	state, err := s.state(ctx)
	if err != nil {
		return gitrepo.State{}, domain.WorkingSetKey{}, err
	}
	if err := state.RequireMutableState(); err != nil {
		return gitrepo.State{}, domain.WorkingSetKey{}, err
	}
	return state, keyForState(state), nil
}

func (s *Service) requireCommitSourceState(ctx context.Context, expected gitrepo.State) error {
	current, err := s.state(ctx)
	if err != nil {
		return err
	}
	if !sameSourceState(expected, current) {
		return domain.NewError(domain.CodeStaleWorkingSet, errors.New("Git source state changed while committing; retry commit"))
	}
	return nil
}

func sameSourceState(expected, current gitrepo.State) bool {
	return expected.Root == current.Root &&
		expected.CommonDir == current.CommonDir &&
		expected.RepositoryID == current.RepositoryID &&
		expected.WorktreeID == current.WorktreeID &&
		expected.Branch == current.Branch &&
		expected.HeadSHA == current.HeadSHA
}

func keyForState(state gitrepo.State) domain.WorkingSetKey {
	refName := state.RefName()
	if state.Branch == "" {
		refName = "refs/contexts/HEAD"
	}
	return domain.WorkingSetKey{RepositoryID: state.RepositoryID, WorktreeID: state.WorktreeID, RefName: refName, SourceSHA: state.HeadSHA}
}

func (s *Service) openStore(ctx context.Context, create bool) (*store.Store, error) {
	if s.store != nil {
		return s.store, nil
	}
	if !create {
		if _, err := os.Stat(s.layout.Database); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, domain.NewError(domain.CodeNotInitialized, errors.New("run thread-keep init first"))
			}
			return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("inspect local context database: %w", err))
		}
	}
	contextStore, err := store.Open(ctx, s.layout)
	if err != nil {
		return nil, err
	}
	s.store = contextStore
	return contextStore, nil
}

func defaultAuthor(ctx context.Context, state gitrepo.State) string {
	if name := strings.TrimSpace(state.UserName(ctx)); name != "" {
		return name
	}
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" {
		return user
	}
	return "unknown"
}
