package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

type FinalizeLandingInput struct {
	Key            domain.WorkingSetKey
	Session        domain.LandingSession
	ExpectedParent string
	Commit         domain.ContextCommit
	Notes          []domain.Note
}

func (s *Store) CreateLandingSession(ctx context.Context, session domain.LandingSession) (domain.LandingSession, error) {
	if err := validateLandingSession(session); err != nil {
		return domain.LandingSession{}, err
	}
	identifier, err := newID()
	if err != nil {
		return domain.LandingSession{}, localError("generate landing session ID", err)
	}
	session.ID = identifier
	session.Version = 1
	if len(session.Plan.Conflicts) == 0 {
		session.State = domain.LandingSessionReady
	} else {
		session.State = domain.LandingSessionOpen
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return domain.LandingSession{}, localError("serialize landing session", err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO landing_sessions (session_id, landing_id, version, state, payload) VALUES (?, ?, ?, ?, ?)", session.ID, session.LandingID, session.Version, session.State, payload); err != nil {
		return domain.LandingSession{}, localError("create landing session", err)
	}
	return session, nil
}

func (s *Store) LandingSession(ctx context.Context, sessionID string) (domain.LandingSession, error) {
	var payload []byte
	if err := s.db.QueryRowContext(ctx, "SELECT payload FROM landing_sessions WHERE session_id = ?", strings.TrimSpace(sessionID)).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.LandingSession{}, domain.NewError(domain.CodeEntityNotFound, errors.New("landing session does not exist"))
		}
		return domain.LandingSession{}, localError("read landing session", err)
	}
	var session domain.LandingSession
	if err := json.Unmarshal(payload, &session); err != nil {
		return domain.LandingSession{}, localError("decode landing session", err)
	}
	return session, nil
}

func (s *Store) UpdateLandingSession(ctx context.Context, expectedVersion int, session domain.LandingSession) (domain.LandingSession, error) {
	if expectedVersion < 1 || session.ID == "" || session.State == domain.LandingSessionCommitted {
		return domain.LandingSession{}, domain.NewError(domain.CodeValidation, errors.New("landing session update is invalid"))
	}
	session.Version = expectedVersion + 1
	if len(session.Plan.Conflicts) == 0 {
		session.State = domain.LandingSessionReady
	} else {
		session.State = domain.LandingSessionOpen
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return domain.LandingSession{}, localError("serialize updated landing session", err)
	}
	result, err := s.db.ExecContext(ctx, "UPDATE landing_sessions SET version = ?, state = ?, payload = ? WHERE session_id = ? AND version = ?", session.Version, session.State, payload, session.ID, expectedVersion)
	if err != nil {
		return domain.LandingSession{}, localError("update landing session", err)
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return domain.LandingSession{}, domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing session changed concurrently"))
	}
	return session, nil
}

func validateLandingSession(session domain.LandingSession) error {
	if session.LandingID == "" || session.RemoteName == "" || session.RepositoryID == "" || session.RefName == "" || session.SourceSHA == "" || session.ExpectedRemoteCommitID == "" || session.ExpectedRemoteRefVersion < 1 || session.Plan.Kind != domain.ContextPlanFinal || session.Plan.Fingerprint.HeadSourceSHA != session.SourceSHA || session.CreatedAt.IsZero() || len(session.Entities) == 0 || len(session.Provenance) == 0 {
		return domain.NewError(domain.CodeValidation, errors.New("landing session is incomplete"))
	}
	return nil
}

func (s *Store) FinalizeLanding(ctx context.Context, input FinalizeLandingInput) error {
	if input.Session.State != domain.LandingSessionReady || input.Commit.ID == "" || input.Commit.ParentID != input.ExpectedParent || input.Commit.SourceSHA != input.Key.SourceSHA || input.Commit.RefName != input.Key.RefName {
		return domain.NewError(domain.CodeValidation, errors.New("landing finalization input is invalid"))
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if err := s.requireWorkingSet(ctx, conn, input.Key); err != nil {
			return err
		}
		var pending int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_notes WHERE worktree_id = ?", input.Key.WorktreeID).Scan(&pending); err != nil {
			return localError("count pending notes before landing", err)
		}
		if pending != 0 {
			return domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit or discard pending context before landing recovery"))
		}
		current, err := currentContextRef(ctx, conn, input.Key.RefName)
		if err != nil {
			return err
		}
		if current.CommitID != input.ExpectedParent {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local context ref changed since landing recovery started"))
		}
		var storedVersion int
		var state domain.LandingSessionState
		if err := conn.QueryRowContext(ctx, "SELECT version, state FROM landing_sessions WHERE session_id = ?", input.Session.ID).Scan(&storedVersion, &state); err != nil {
			return localError("read landing session before commit", err)
		}
		if storedVersion != input.Session.Version || state != domain.LandingSessionReady {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing session changed before commit"))
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO context_commits (commit_id, parent_id, ref_name, source_sha, message, author, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, input.Commit.ID, input.Commit.ParentID, input.Commit.RefName, input.Commit.SourceSHA, input.Commit.Message, input.Commit.Author, input.Commit.CreatedAt.UnixNano()); err != nil {
			return localError("insert landing context commit", err)
		}
		if err := insertContextCommitParents(ctx, conn, input.Commit.ID, []string{input.ExpectedParent}); err != nil {
			return err
		}
		for _, note := range input.Notes {
			topics, err := encodeNoteTopics(note.Topics)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO committed_notes (context_commit_id, note_id, revision_id, supersedes_revision_id, entity_key, kind, body, author, origin, created_at, binding_state, binding_source_sha, review_reason, topics_json)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, input.Commit.ID, note.ID, note.RevisionID, note.SupersedesRevisionID, note.EntityKey, note.Kind, note.Body, note.Author, note.Origin, note.CreatedAt.UnixNano(), note.BindingState, note.BindingSourceSHA, note.ReviewReason, topics); err != nil {
				return localError("insert landing context note", err)
			}
		}
		result, err := conn.ExecContext(ctx, "UPDATE context_refs SET commit_id = ?, source_sha = ?, version = version + 1 WHERE ref_name = ? AND commit_id = ? AND version = ?", input.Commit.ID, input.Commit.SourceSHA, input.Key.RefName, input.ExpectedParent, current.Version)
		if err != nil {
			return localError("advance local landing ref", err)
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local landing ref compare-and-swap failed"))
		}
		input.Session.State = domain.LandingSessionCommitted
		input.Session.Version++
		payload, err := json.Marshal(input.Session)
		if err != nil {
			return localError("serialize committed landing session", err)
		}
		result, err = conn.ExecContext(ctx, "UPDATE landing_sessions SET version = ?, state = ?, payload = ? WHERE session_id = ? AND version = ?", input.Session.Version, input.Session.State, payload, input.Session.ID, storedVersion)
		if err != nil {
			return localError("mark landing session committed", err)
		}
		changed, _ = result.RowsAffected()
		if changed != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing session changed during commit"))
		}
		if _, err := conn.ExecContext(ctx, "UPDATE working_sets SET base_context_commit_id = ? WHERE worktree_id = ?", input.Commit.ID, input.Key.WorktreeID); err != nil {
			return localError("advance landing working set", err)
		}
		return s.rebuildSearch(ctx, conn, input.Key)
	})
}
