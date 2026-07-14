package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tae2089/thread-keep/internal/domain"
)

type CommitSnapshot struct {
	Entities       []domain.Entity
	CommittedNotes []domain.Note
	PendingNotes   []domain.Note
	ParentID       string
	WorkingSource  string
}

type FinalizeInput struct {
	Key            domain.WorkingSetKey
	ExpectedParent string
	PendingNoteIDs []string
	Commit         domain.ContextCommit
	Notes          []domain.Note
}

type FinalizeMergeInput struct {
	Key       domain.WorkingSetKey
	SessionID string
	Commit    domain.ContextCommit
	ParentIDs []string
	Notes     []domain.Note
}

type RebuildInput struct {
	Key         domain.WorkingSetKey
	CommitID    string
	Projections []domain.LanguageProjection
}

func (s *Store) CommitSnapshot(ctx context.Context, key domain.WorkingSetKey) (CommitSnapshot, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return CommitSnapshot{}, localError("open SQLite connection", err)
	}
	defer conn.Close()
	if err := s.requireWorkingSet(ctx, conn, key); err != nil {
		return CommitSnapshot{}, err
	}
	var snapshot CommitSnapshot
	if err := conn.QueryRowContext(ctx, "SELECT base_context_commit_id, source_sha FROM working_sets WHERE worktree_id = ?", key.WorktreeID).Scan(&snapshot.ParentID, &snapshot.WorkingSource); err != nil {
		return CommitSnapshot{}, localError("read working set", err)
	}
	entities, err := listFreshEntities(ctx, conn, key.WorktreeID, snapshot.WorkingSource)
	if err != nil {
		return CommitSnapshot{}, err
	}
	snapshot.Entities = entities
	if snapshot.ParentID != "" {
		notes, err := listCommittedNotes(ctx, conn, snapshot.ParentID)
		if err != nil {
			return CommitSnapshot{}, err
		}
		snapshot.CommittedNotes = notes
	}
	pending, err := listPendingNotes(ctx, conn, key.WorktreeID)
	if err != nil {
		return CommitSnapshot{}, err
	}
	snapshot.PendingNotes = pending
	return snapshot, nil
}

func (s *Store) FinalizeCommit(ctx context.Context, input FinalizeInput) error {
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		var baseParent, sourceSHA, refName string
		if err := conn.QueryRowContext(ctx, "SELECT base_context_commit_id, source_sha, ref_name FROM working_sets WHERE worktree_id = ?", input.Key.WorktreeID).Scan(&baseParent, &sourceSHA, &refName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.NewError(domain.CodeStaleWorkingSet, errors.New("working set does not exist"))
			}
			return localError("read working set before commit", err)
		}
		if sourceSHA != input.Key.SourceSHA || refName != input.Key.RefName || baseParent != input.ExpectedParent {
			return domain.NewError(domain.CodeStaleWorkingSet, errors.New("working set identity changed"))
		}
		var pendingCount int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_notes WHERE worktree_id = ?", input.Key.WorktreeID).Scan(&pendingCount); err != nil {
			return localError("count pending notes", err)
		}
		if pendingCount == 0 {
			return domain.NewError(domain.CodeNothingToCommit, errors.New("no pending context changes"))
		}
		currentPendingIDs, err := pendingNoteIDs(ctx, conn, input.Key.WorktreeID)
		if err != nil {
			return err
		}
		if !sameIDs(currentPendingIDs, input.PendingNoteIDs) {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("pending context changes changed while committing"))
		}
		currentParent, version, err := s.ref(ctx, conn, input.Key.RefName)
		if err != nil {
			return err
		}
		if currentParent != input.ExpectedParent {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("context ref changed while committing"))
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO context_commits (commit_id, parent_id, ref_name, source_sha, message, author, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, input.Commit.ID, input.Commit.ParentID, input.Commit.RefName, input.Commit.SourceSHA, input.Commit.Message, input.Commit.Author, input.Commit.CreatedAt.UnixNano()); err != nil {
			return localError("insert context commit", err)
		}
		if err := insertContextCommitParents(ctx, conn, input.Commit.ID, []string{input.Commit.ParentID}); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM committed_notes WHERE context_commit_id = ?", input.Commit.ID); err != nil {
			return localError("clear committed note snapshot", err)
		}
		for _, note := range input.Notes {
			topics, err := encodeNoteTopics(note.Topics)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO committed_notes (context_commit_id, note_id, revision_id, supersedes_revision_id, entity_key, kind, body, author, origin, created_at, binding_state, binding_source_sha, review_reason, topics_json)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, input.Commit.ID, note.ID, note.RevisionID, note.SupersedesRevisionID, note.EntityKey, note.Kind, note.Body, note.Author, note.Origin, note.CreatedAt.UnixNano(), note.BindingState, note.BindingSourceSHA, note.ReviewReason, topics); err != nil {
				return localError("insert committed note", err)
			}
		}
		if currentParent == "" {
			if _, err := conn.ExecContext(ctx, "INSERT INTO context_refs (ref_name, commit_id, source_sha, version) VALUES (?, ?, ?, 1)", input.Key.RefName, input.Commit.ID, input.Commit.SourceSHA); err != nil {
				return localError("create context ref", err)
			}
		} else {
			result, err := conn.ExecContext(ctx, "UPDATE context_refs SET commit_id = ?, source_sha = ?, version = version + 1 WHERE ref_name = ? AND version = ? AND commit_id = ?", input.Commit.ID, input.Commit.SourceSHA, input.Key.RefName, version, input.ExpectedParent)
			if err != nil {
				return localError("advance context ref", err)
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return localError("check context ref CAS", err)
			}
			if changed != 1 {
				return domain.NewError(domain.CodeConcurrentUpdate, errors.New("context ref compare-and-swap failed"))
			}
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM pending_notes WHERE worktree_id = ?", input.Key.WorktreeID); err != nil {
			return localError("clear pending notes", err)
		}
		if _, err := conn.ExecContext(ctx, "UPDATE working_sets SET base_context_commit_id = ? WHERE worktree_id = ?", input.Commit.ID, input.Key.WorktreeID); err != nil {
			return localError("advance working set parent", err)
		}
		return s.rebuildSearch(ctx, conn, input.Key)
	})
	if err == nil {
		return nil
	}
	confirmed, confirmErr := s.commitWasFinalized(ctx, input)
	if confirmErr == nil && confirmed {
		return nil
	}
	return err
}

func (s *Store) FinalizeMerge(ctx context.Context, input FinalizeMergeInput) error {
	if len(input.ParentIDs) != 2 || input.ParentIDs[0] == "" || input.ParentIDs[1] == "" || input.ParentIDs[0] == input.ParentIDs[1] {
		return domain.NewError(domain.CodeValidation, errors.New("merge finalization requires two distinct ordered parents"))
	}
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		if err := s.requireWorkingSet(ctx, conn, input.Key); err != nil {
			return err
		}
		var localID, remoteID, sourceSHA string
		var state domain.MergeSessionState
		err := conn.QueryRowContext(ctx, "SELECT local_snapshot_id, remote_snapshot_id, source_sha, state FROM merge_sessions WHERE session_id = ?", input.SessionID).Scan(&localID, &remoteID, &sourceSHA, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("merge session %q does not exist", input.SessionID))
		}
		if err != nil {
			return localError("read merge session before finalization", err)
		}
		if state != domain.MergeSessionReady {
			return domain.NewError(domain.CodeValidation, errors.New("merge session is not ready to commit"))
		}
		if localID != input.ParentIDs[0] || remoteID != input.ParentIDs[1] || input.Commit.ParentID != localID || input.Commit.SourceSHA != sourceSHA || input.Commit.SourceSHA != input.Key.SourceSHA || input.Commit.RefName != input.Key.RefName {
			return domain.NewError(domain.CodeValidation, errors.New("merge finalization does not match its session or working set"))
		}
		var pendingCount int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_notes WHERE worktree_id = ?", input.Key.WorktreeID).Scan(&pendingCount); err != nil {
			return localError("count pending notes before merge", err)
		}
		if pendingCount != 0 {
			return domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit or discard pending context changes before merging"))
		}
		actual, err := currentContextRef(ctx, conn, input.Key.RefName)
		if err != nil {
			return err
		}
		if actual.CommitID != localID || actual.SourceSHA != input.Key.SourceSHA {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local context ref changed while merging"))
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO context_commits (commit_id, parent_id, ref_name, source_sha, message, author, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, input.Commit.ID, input.Commit.ParentID, input.Commit.RefName, input.Commit.SourceSHA, input.Commit.Message, input.Commit.Author, input.Commit.CreatedAt.UnixNano()); err != nil {
			return localError("insert merged context commit", err)
		}
		if err := insertContextCommitParents(ctx, conn, input.Commit.ID, input.ParentIDs); err != nil {
			return err
		}
		for _, note := range input.Notes {
			topics, err := encodeNoteTopics(note.Topics)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO committed_notes (context_commit_id, note_id, revision_id, supersedes_revision_id, entity_key, kind, body, author, origin, created_at, binding_state, binding_source_sha, review_reason, topics_json)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, input.Commit.ID, note.ID, note.RevisionID, note.SupersedesRevisionID, note.EntityKey, note.Kind, note.Body, note.Author, note.Origin, note.CreatedAt.UnixNano(), note.BindingState, note.BindingSourceSHA, note.ReviewReason, topics); err != nil {
				return localError("insert merged context note", err)
			}
		}
		result, err := conn.ExecContext(ctx, "UPDATE context_refs SET commit_id = ?, source_sha = ?, version = version + 1 WHERE ref_name = ? AND commit_id = ? AND source_sha = ? AND version = ?", input.Commit.ID, input.Commit.SourceSHA, input.Key.RefName, localID, input.Key.SourceSHA, actual.Version)
		if err != nil {
			return localError("advance merged context ref", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return localError("check merged context ref compare-and-swap", err)
		}
		if changed != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local context ref compare-and-swap failed while merging"))
		}
		result, err = conn.ExecContext(ctx, "UPDATE working_sets SET base_context_commit_id = ? WHERE worktree_id = ? AND ref_name = ? AND source_sha = ?", input.Commit.ID, input.Key.WorktreeID, input.Key.RefName, input.Key.SourceSHA)
		if err != nil {
			return localError("advance merged working set parent", err)
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return localError("check merged working set", err)
		}
		if changed != 1 {
			return domain.NewError(domain.CodeStaleWorkingSet, errors.New("working set changed while merging"))
		}
		if _, err := conn.ExecContext(ctx, "UPDATE merge_sessions SET state = ? WHERE session_id = ? AND state = ?", domain.MergeSessionCommitted, input.SessionID, domain.MergeSessionReady); err != nil {
			return localError("mark merge session committed", err)
		}
		return s.rebuildSearch(ctx, conn, input.Key)
	})
	return err
}

func (s *Store) commitWasFinalized(ctx context.Context, input FinalizeInput) (bool, error) {
	var commitCount, pendingCount int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM context_commits WHERE commit_id = ?", input.Commit.ID).Scan(&commitCount); err != nil {
		return false, localError("confirm context commit", err)
	}
	var refID string
	err := s.db.QueryRowContext(ctx, "SELECT commit_id FROM context_refs WHERE ref_name = ?", input.Key.RefName).Scan(&refID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, localError("confirm context ref", err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_notes WHERE worktree_id = ?", input.Key.WorktreeID).Scan(&pendingCount); err != nil {
		return false, localError("confirm pending notes", err)
	}
	return commitCount == 1 && refID == input.Commit.ID && pendingCount == 0, nil
}

func insertContextCommitParents(ctx context.Context, conn *sql.Conn, commitID string, parentIDs []string) error {
	for index, parentID := range parentIDs {
		if parentID == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO context_commit_parents (context_commit_id, parent_index, parent_id)
			VALUES (?, ?, ?)`, commitID, index, parentID); err != nil {
			return localError("record ordered context parent", err)
		}
	}
	return nil
}
