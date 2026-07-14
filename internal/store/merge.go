package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

func (s *Store) CreateMergeSession(ctx context.Context, session domain.MergeSession) (domain.MergeSession, error) {
	for index := range session.Conflicts {
		if session.Conflicts[index].ID == "" {
			session.Conflicts[index].ID = session.Conflicts[index].NoteID
		}
	}
	if err := validateMergeSession(session); err != nil {
		return domain.MergeSession{}, err
	}
	identifier, err := newID()
	if err != nil {
		return domain.MergeSession{}, localError("generate merge session ID", err)
	}
	session.ID = identifier
	if len(session.Conflicts) == 0 {
		session.State = domain.MergeSessionReady
	} else {
		session.State = domain.MergeSessionOpen
		for index := range session.Conflicts {
			session.Conflicts[index].Resolution = domain.MergeConflictUnresolved
			session.Conflicts[index].Authored = nil
		}
	}
	provenance, err := json.Marshal(session.Provenance)
	if err != nil {
		return domain.MergeSession{}, localError("serialize merge session provenance", err)
	}
	automaticRecords, err := json.Marshal(session.AutomaticRecords)
	if err != nil {
		return domain.MergeSession{}, localError("serialize merge session records", err)
	}
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO merge_sessions (session_id, local_snapshot_id, remote_snapshot_id, base_snapshot_id, repository_id, ref_name, source_sha, provenance_json, message, author, planned_created_at, state, automatic_records_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, session.ID, session.LocalSnapshotID, session.RemoteSnapshotID, session.BaseSnapshotID, session.RepositoryID, session.RefName, session.SourceSHA, provenance, session.Message, session.Author, session.PlannedCreatedAt.UnixNano(), session.State, automaticRecords); err != nil {
			return localError("write merge session", err)
		}
		for _, conflict := range session.Conflicts {
			payload, err := json.Marshal(conflict)
			if err != nil {
				return localError("serialize merge conflict", err)
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO merge_conflicts (session_id, conflict_id, conflict_json) VALUES (?, ?, ?)", session.ID, conflict.ID, payload); err != nil {
				return localError("write merge conflict", err)
			}
		}
		return nil
	})
	if err != nil {
		return domain.MergeSession{}, err
	}
	return session, nil
}

func (s *Store) MergeSession(ctx context.Context, sessionID string) (domain.MergeSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return domain.MergeSession{}, domain.NewError(domain.CodeValidation, errors.New("merge session ID must not be empty"))
	}
	var session domain.MergeSession
	var provenance, automaticRecords []byte
	var plannedCreatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT session_id, local_snapshot_id, remote_snapshot_id, base_snapshot_id, repository_id, ref_name, source_sha, provenance_json, message, author, planned_created_at, state, automatic_records_json
		FROM merge_sessions WHERE session_id = ?`, sessionID).Scan(&session.ID, &session.LocalSnapshotID, &session.RemoteSnapshotID, &session.BaseSnapshotID, &session.RepositoryID, &session.RefName, &session.SourceSHA, &provenance, &session.Message, &session.Author, &plannedCreatedAt, &session.State, &automaticRecords)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.MergeSession{}, domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("merge session %q does not exist", sessionID))
	}
	if err != nil {
		return domain.MergeSession{}, localError("read merge session", err)
	}
	if err := json.Unmarshal(provenance, &session.Provenance); err != nil {
		return domain.MergeSession{}, localError("decode merge session provenance", err)
	}
	if err := json.Unmarshal(automaticRecords, &session.AutomaticRecords); err != nil {
		return domain.MergeSession{}, localError("decode merge session records", err)
	}
	session.PlannedCreatedAt = time.Unix(0, plannedCreatedAt).UTC()
	rows, err := s.db.QueryContext(ctx, "SELECT conflict_id, conflict_json FROM merge_conflicts WHERE session_id = ? ORDER BY conflict_id", session.ID)
	if err != nil {
		return domain.MergeSession{}, localError("list merge conflicts", err)
	}
	defer rows.Close()
	for rows.Next() {
		var identifier string
		var payload []byte
		if err := rows.Scan(&identifier, &payload); err != nil {
			return domain.MergeSession{}, localError("scan merge conflict", err)
		}
		var conflict domain.MergeSessionConflict
		if err := json.Unmarshal(payload, &conflict); err != nil {
			return domain.MergeSession{}, localError("decode merge conflict", err)
		}
		if conflict.ID != identifier {
			return domain.MergeSession{}, localError("validate stored merge conflict", errors.New("merge conflict ID does not match its storage key"))
		}
		session.Conflicts = append(session.Conflicts, conflict)
	}
	if err := rows.Err(); err != nil {
		return domain.MergeSession{}, localError("iterate merge conflicts", err)
	}
	return session, nil
}

func (s *Store) ResolveMergeConflict(ctx context.Context, sessionID, conflictID string, resolution domain.MergeConflictResolution, authored *domain.SnapshotMergeRecord) (domain.MergeSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	conflictID = strings.TrimSpace(conflictID)
	if sessionID == "" || conflictID == "" {
		return domain.MergeSession{}, domain.NewError(domain.CodeValidation, errors.New("merge session and conflict IDs must not be empty"))
	}
	if resolution != domain.MergeConflictLocal && resolution != domain.MergeConflictRemote && resolution != domain.MergeConflictAuthored {
		return domain.MergeSession{}, domain.NewError(domain.CodeValidation, errors.New("merge conflict resolution is invalid"))
	}
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		var state domain.MergeSessionState
		var sourceSHA string
		err := conn.QueryRowContext(ctx, "SELECT state, source_sha FROM merge_sessions WHERE session_id = ?", sessionID).Scan(&state, &sourceSHA)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("merge session %q does not exist", sessionID))
		}
		if err != nil {
			return localError("read merge session before resolution", err)
		}
		if state == domain.MergeSessionCommitted {
			return domain.NewError(domain.CodeValidation, errors.New("committed merge sessions cannot be resolved"))
		}
		rows, err := conn.QueryContext(ctx, "SELECT conflict_id, conflict_json FROM merge_conflicts WHERE session_id = ? ORDER BY conflict_id", sessionID)
		if err != nil {
			return localError("list merge conflicts before resolution", err)
		}
		defer rows.Close()
		var conflicts []domain.MergeSessionConflict
		found := false
		for rows.Next() {
			var identifier string
			var payload []byte
			if err := rows.Scan(&identifier, &payload); err != nil {
				return localError("scan merge conflict before resolution", err)
			}
			var conflict domain.MergeSessionConflict
			if err := json.Unmarshal(payload, &conflict); err != nil {
				return localError("decode merge conflict before resolution", err)
			}
			if conflict.ID != identifier {
				return localError("validate stored merge conflict before resolution", errors.New("merge conflict ID does not match its storage key"))
			}
			if identifier == conflictID {
				found = true
				if conflict.Resolution != domain.MergeConflictUnresolved {
					return domain.NewError(domain.CodeValidation, errors.New("merge conflict is already resolved"))
				}
				switch resolution {
				case domain.MergeConflictLocal:
					if conflict.Local == nil {
						return domain.NewError(domain.CodeValidation, errors.New("merge conflict has no local record"))
					}
				case domain.MergeConflictRemote:
					if conflict.Remote == nil {
						return domain.NewError(domain.CodeValidation, errors.New("merge conflict has no remote record"))
					}
				case domain.MergeConflictAuthored:
					if !validAuthoredMergeRecord(conflict.NoteID, authored) || authored.Note.BindingSourceSHA != sourceSHA {
						return domain.NewError(domain.CodeValidation, errors.New("authored merge resolution is invalid"))
					}
					conflict.Authored = authored
				}
				conflict.Resolution = resolution
			}
			conflicts = append(conflicts, conflict)
		}
		if err := rows.Err(); err != nil {
			return localError("iterate merge conflicts before resolution", err)
		}
		if !found {
			return domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("merge conflict %q does not exist", conflictID))
		}
		ready := true
		for _, conflict := range conflicts {
			if conflict.Resolution == domain.MergeConflictUnresolved {
				ready = false
			}
			payload, err := json.Marshal(conflict)
			if err != nil {
				return localError("serialize resolved merge conflict", err)
			}
			if _, err := conn.ExecContext(ctx, "UPDATE merge_conflicts SET conflict_json = ? WHERE session_id = ? AND conflict_id = ?", payload, sessionID, conflict.ID); err != nil {
				return localError("write resolved merge conflict", err)
			}
		}
		nextState := domain.MergeSessionOpen
		if ready {
			nextState = domain.MergeSessionReady
		}
		if _, err := conn.ExecContext(ctx, "UPDATE merge_sessions SET state = ? WHERE session_id = ?", nextState, sessionID); err != nil {
			return localError("advance merge session state", err)
		}
		return nil
	})
	if err != nil {
		return domain.MergeSession{}, err
	}
	return s.MergeSession(ctx, sessionID)
}

func validAuthoredMergeRecord(noteID string, record *domain.SnapshotMergeRecord) bool {
	if record == nil || record.Note.ID != noteID || record.Note.RevisionID == "" || record.Note.EntityKey == "" || !domain.ValidNoteKind(record.Note.Kind) || strings.TrimSpace(record.Note.Body) == "" || record.Note.Author == "" || record.Note.Origin == "" || record.Note.CreatedAt.IsZero() || !domain.ValidNoteBindingState(record.Note.BindingState) || record.Note.BindingSourceSHA == "" {
		return false
	}
	mapping := record.Mapping
	return mapping.EntityKey == record.Note.EntityKey && mapping.NoteID == record.Note.ID && mapping.RevisionID == record.Note.RevisionID && mapping.BindingState == record.Note.BindingState && mapping.BindingSourceSHA == record.Note.BindingSourceSHA && mapping.ReviewReason == record.Note.ReviewReason
}

func validateMergeSession(session domain.MergeSession) error {
	for _, identifier := range []string{session.LocalSnapshotID, session.RemoteSnapshotID, session.BaseSnapshotID} {
		if _, err := domain.NormalizeContextCommitID(identifier); err != nil {
			return err
		}
	}
	if strings.TrimSpace(session.RepositoryID) == "" || strings.TrimSpace(session.RefName) == "" || strings.TrimSpace(session.SourceSHA) == "" || strings.TrimSpace(session.Message) == "" || strings.TrimSpace(session.Author) == "" || session.PlannedCreatedAt.IsZero() || len(session.Provenance) == 0 {
		return domain.NewError(domain.CodeValidation, errors.New("merge session is incomplete"))
	}
	conflictIDs := make(map[string]struct{}, len(session.Conflicts))
	noteIDs := make(map[string]struct{}, len(session.Conflicts))
	for _, conflict := range session.Conflicts {
		if strings.TrimSpace(conflict.NoteID) == "" || strings.TrimSpace(conflict.ID) == "" {
			return domain.NewError(domain.CodeValidation, errors.New("merge conflict is missing its note ID"))
		}
		if _, found := conflictIDs[conflict.ID]; found {
			return domain.NewError(domain.CodeValidation, errors.New("merge session has duplicate conflict IDs"))
		}
		if _, found := noteIDs[conflict.NoteID]; found {
			return domain.NewError(domain.CodeValidation, errors.New("merge session has duplicate conflict note IDs"))
		}
		conflictIDs[conflict.ID] = struct{}{}
		noteIDs[conflict.NoteID] = struct{}{}
	}
	return nil
}
