package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

func (s *Store) AddPendingNote(ctx context.Context, key domain.WorkingSetKey, note domain.Note) (domain.Note, error) {
	if note.ID == "" {
		identifier, err := newID()
		if err != nil {
			return domain.Note{}, localError("generate note ID", err)
		}
		note.ID = identifier
	}
	if note.RevisionID == "" {
		identifier, err := newID()
		if err != nil {
			return domain.Note{}, localError("generate note revision ID", err)
		}
		note.RevisionID = identifier
	}
	if note.BindingState == "" {
		note.BindingState = domain.NoteBindingActive
	}
	if note.BindingSourceSHA == "" {
		note.BindingSourceSHA = key.SourceSHA
	}
	note.Pending = true
	if note.CreatedAt.IsZero() {
		note.CreatedAt = time.Now().UTC()
	}
	if note.Origin == "" {
		note.Origin = "human"
	}
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		if err := s.requireWorkingSet(ctx, conn, key); err != nil {
			return err
		}
		if _, err := entityByKeyFresh(ctx, conn, key.WorktreeID, key.SourceSHA, note.EntityKey); err != nil {
			return err
		}
		if err := upsertPendingNote(ctx, conn, key.WorktreeID, note); err != nil {
			return err
		}
		return s.rebuildSearch(ctx, conn, key)
	})
	if err != nil {
		return domain.Note{}, err
	}
	return note, nil
}

func (s *Store) ReviseNote(ctx context.Context, key domain.WorkingSetKey, noteID, body, author, origin string, topics []string) (domain.Note, error) {
	var revised domain.Note
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		if err := s.requireWorkingSet(ctx, conn, key); err != nil {
			return err
		}
		var parentID string
		if err := conn.QueryRowContext(ctx, "SELECT base_context_commit_id FROM working_sets WHERE worktree_id = ?", key.WorktreeID).Scan(&parentID); err != nil {
			return localError("read note parent", err)
		}
		if parentID == "" {
			return domain.NewError(domain.CodeValidation, errors.New("commit a note before revising it"))
		}
		parent, found, err := committedNoteByID(ctx, conn, parentID, noteID)
		if err != nil {
			return err
		}
		if !found {
			return domain.NewError(domain.CodeValidation, errors.New("commit a note before revising it"))
		}
		current := parent
		pending, pendingFound, err := pendingNoteByID(ctx, conn, key.WorktreeID, noteID)
		if err != nil {
			return err
		}
		if pendingFound {
			if pending.RevisionID != parent.RevisionID {
				return domain.NewError(domain.CodeValidation, errors.New("commit a pending note revision before revising it"))
			}
			current = pending
		}
		if current.BindingState != domain.NoteBindingActive && current.BindingState != domain.NoteBindingNeedsReview {
			return domain.NewError(domain.CodeValidation, errors.New("only active or needs-review notes can be revised"))
		}
		if _, err := entityByKeyFresh(ctx, conn, key.WorktreeID, key.SourceSHA, current.EntityKey); err != nil {
			return err
		}
		revisionID, err := newID()
		if err != nil {
			return localError("generate note revision ID", err)
		}
		revised = current
		revised.RevisionID = revisionID
		revised.SupersedesRevisionID = current.RevisionID
		revised.Body = body
		revised.Author = author
		revised.Origin = origin
		if topics != nil {
			revised.Topics = append([]string(nil), topics...)
		}
		revised.CreatedAt = time.Now().UTC()
		revised.BindingState = domain.NoteBindingActive
		revised.BindingSourceSHA = key.SourceSHA
		revised.ReviewReason = ""
		revised.Pending = true
		if err := upsertPendingNote(ctx, conn, key.WorktreeID, revised); err != nil {
			return err
		}
		return s.rebuildSearch(ctx, conn, key)
	})
	if err != nil {
		return domain.Note{}, err
	}
	return revised, nil
}

func (s *Store) ReviewNote(ctx context.Context, key domain.WorkingSetKey, noteID, entityKey string) (domain.Note, error) {
	var confirmed domain.Note
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		if err := s.requireWorkingSet(ctx, conn, key); err != nil {
			return err
		}
		current, err := effectiveNote(ctx, conn, key, noteID)
		if err != nil {
			return err
		}
		if current.BindingState != domain.NoteBindingNeedsReview {
			return domain.NewError(domain.CodeValidation, errors.New("only needs-review notes can be confirmed"))
		}
		if _, err := entityByKeyFresh(ctx, conn, key.WorktreeID, key.SourceSHA, entityKey); err != nil {
			return err
		}
		confirmed = current
		confirmed.EntityKey = entityKey
		confirmed.BindingState = domain.NoteBindingActive
		confirmed.BindingSourceSHA = key.SourceSHA
		confirmed.ReviewReason = ""
		confirmed.Pending = true
		if err := upsertPendingNote(ctx, conn, key.WorktreeID, confirmed); err != nil {
			return err
		}
		return s.rebuildSearch(ctx, conn, key)
	})
	if err != nil {
		return domain.Note{}, err
	}
	return confirmed, nil
}

func upsertPendingNote(ctx context.Context, conn *sql.Conn, worktreeID string, note domain.Note) error {
	topicsValue, err := domain.NormalizeNoteTopics(note.Topics)
	if err != nil {
		return err
	}
	note.Topics = topicsValue
	topics, err := encodeNoteTopics(note.Topics)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO pending_notes (worktree_id, note_id, revision_id, supersedes_revision_id, entity_key, kind, body, author, origin, created_at, binding_state, binding_source_sha, review_reason, topics_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(worktree_id, note_id) DO UPDATE SET revision_id = excluded.revision_id, supersedes_revision_id = excluded.supersedes_revision_id, entity_key = excluded.entity_key, kind = excluded.kind, body = excluded.body, author = excluded.author, origin = excluded.origin, created_at = excluded.created_at, binding_state = excluded.binding_state, binding_source_sha = excluded.binding_source_sha, review_reason = excluded.review_reason, topics_json = excluded.topics_json`, worktreeID, note.ID, note.RevisionID, note.SupersedesRevisionID, note.EntityKey, note.Kind, note.Body, note.Author, note.Origin, note.CreatedAt.UnixNano(), note.BindingState, note.BindingSourceSHA, note.ReviewReason, topics); err != nil {
		return localError("upsert pending note", err)
	}
	return nil
}

func encodeNoteTopics(topics []string) (string, error) {
	encoded, err := json.Marshal(topics)
	if err != nil {
		return "", localError("encode note topics", err)
	}
	return string(encoded), nil
}

func effectiveNote(ctx context.Context, conn *sql.Conn, key domain.WorkingSetKey, noteID string) (domain.Note, error) {
	note, found, err := pendingNoteByID(ctx, conn, key.WorktreeID, noteID)
	if err != nil || found {
		return note, err
	}
	var parentID string
	if err := conn.QueryRowContext(ctx, "SELECT base_context_commit_id FROM working_sets WHERE worktree_id = ?", key.WorktreeID).Scan(&parentID); err != nil {
		return domain.Note{}, localError("read note parent", err)
	}
	if parentID == "" {
		return domain.Note{}, domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("note %q does not exist", noteID))
	}
	note, found, err = committedNoteByID(ctx, conn, parentID, noteID)
	if err != nil {
		return domain.Note{}, err
	}
	if !found {
		return domain.Note{}, domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("note %q does not exist", noteID))
	}
	return note, nil
}

func pendingNoteByID(ctx context.Context, conn *sql.Conn, worktreeID, noteID string) (domain.Note, bool, error) {
	row := conn.QueryRowContext(ctx, `SELECT p.note_id, p.revision_id, p.supersedes_revision_id, p.entity_key, p.kind, p.body, p.author, p.origin, p.created_at, p.binding_state, p.binding_source_sha, p.review_reason, p.topics_json, w.source_sha
		FROM pending_notes AS p JOIN working_sets AS w ON w.worktree_id = p.worktree_id WHERE p.worktree_id = ? AND p.note_id = ?`, worktreeID, noteID)
	return scanNote(row, true)
}

func committedNoteByID(ctx context.Context, conn *sql.Conn, commitID, noteID string) (domain.Note, bool, error) {
	row := conn.QueryRowContext(ctx, `SELECT n.note_id, n.revision_id, n.supersedes_revision_id, n.entity_key, n.kind, n.body, n.author, n.origin, n.created_at, n.binding_state, n.binding_source_sha, n.review_reason, n.topics_json, c.source_sha
		FROM committed_notes AS n JOIN context_commits AS c ON c.commit_id = n.context_commit_id WHERE n.context_commit_id = ? AND n.note_id = ?`, commitID, noteID)
	return scanNote(row, false)
}

func validateProjection(key domain.WorkingSetKey, coverage domain.Coverage, entities []domain.Entity) error {
	if coverage.Language == "" || coverage.SourceSHA != key.SourceSHA {
		return domain.NewError(domain.CodeValidation, errors.New("coverage must identify the current language and source revision"))
	}
	if coverage.State != domain.CoverageIndexed && len(entities) != 0 {
		return domain.NewError(domain.CodeValidation, errors.New("only indexed coverage may include entities"))
	}
	for _, entity := range entities {
		if entity.Key == "" || entity.Language != coverage.Language || entity.Path == "" || entity.SourceSHA != key.SourceSHA {
			return domain.NewError(domain.CodeValidation, fmt.Errorf("invalid %s entity projection", coverage.Language))
		}
	}
	return nil
}

func effectiveNotes(committed, pending []domain.Note) []domain.Note {
	byID := make(map[string]domain.Note, len(committed)+len(pending))
	for _, note := range committed {
		byID[note.ID] = note
	}
	for _, note := range pending {
		byID[note.ID] = note
	}
	notes := make([]domain.Note, 0, len(byID))
	for _, note := range byID {
		notes = append(notes, note)
	}
	sort.Slice(notes, func(left, right int) bool {
		if notes[left].CreatedAt.Equal(notes[right].CreatedAt) {
			return notes[left].ID < notes[right].ID
		}
		return notes[left].CreatedAt.Before(notes[right].CreatedAt)
	})
	return notes
}
