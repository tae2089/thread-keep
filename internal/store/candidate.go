package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/zeebo/blake3"
)

func (s *Store) ImportCandidate(ctx context.Context, candidate domain.Candidate, notes []domain.CandidateNote) (bool, error) {
	if err := validateCandidateInput(candidate, notes); err != nil {
		return false, err
	}
	payloadHash, err := candidatePayloadHash(candidate, notes)
	if err != nil {
		return false, err
	}
	imported := false
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		var updatedAt int64
		var existingHash string
		err := conn.QueryRowContext(ctx, "SELECT updated_at, payload_hash FROM candidates WHERE candidate_id = ?", candidate.ID).Scan(&updatedAt, &existingHash)
		if errors.Is(err, sql.ErrNoRows) {
			if err := upsertCandidate(ctx, conn, candidate, payloadHash); err != nil {
				return err
			}
			if err := insertCandidateNotes(ctx, conn, notes); err != nil {
				return err
			}
			imported = true
			return nil
		}
		if err != nil {
			return localError("read candidate snapshot", err)
		}
		if updatedAt > candidate.UpdatedAt.UnixNano() {
			return domain.NewError(domain.CodeValidation, errors.New("candidate import is older than the stored provider update"))
		}
		if updatedAt == candidate.UpdatedAt.UnixNano() {
			if existingHash == payloadHash {
				return nil
			}
			return domain.NewError(domain.CodeValidation, errors.New("candidate import has the same provider update time with different contents"))
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM candidate_notes WHERE candidate_id = ? AND state = ?", candidate.ID, domain.CandidateNoteDraft); err != nil {
			return localError("replace candidate draft notes", err)
		}
		if err := upsertCandidate(ctx, conn, candidate, payloadHash); err != nil {
			return err
		}
		if err := insertCandidateNotes(ctx, conn, notes); err != nil {
			return err
		}
		imported = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return imported, nil
}

func (s *Store) Candidate(ctx context.Context, candidateID string) (domain.Candidate, []domain.CandidateNote, error) {
	var candidate domain.Candidate
	var updatedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT candidate_id, provider, repository, number, state, base_sha, head_sha, merge_sha, updated_at
		FROM candidates WHERE candidate_id = ?`, candidateID).Scan(&candidate.ID, &candidate.Provider, &candidate.Repository, &candidate.Number, &candidate.State, &candidate.BaseSHA, &candidate.HeadSHA, &candidate.MergeSHA, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Candidate{}, nil, domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("candidate %q does not exist", candidateID))
	}
	if err != nil {
		return domain.Candidate{}, nil, localError("read candidate", err)
	}
	candidate.UpdatedAt = time.Unix(0, updatedAt).UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT candidate_id, note_id, entity_key, structural_hash, kind, body, author, origin, created_at, state, promoted_note_id
		FROM candidate_notes WHERE candidate_id = ? ORDER BY created_at, note_id`, candidateID)
	if err != nil {
		return domain.Candidate{}, nil, localError("list candidate notes", err)
	}
	defer rows.Close()
	var notes []domain.CandidateNote
	for rows.Next() {
		var note domain.CandidateNote
		var createdAt int64
		if err := rows.Scan(&note.CandidateID, &note.ID, &note.EntityKey, &note.StructuralHash, &note.Kind, &note.Body, &note.Author, &note.Origin, &createdAt, &note.State, &note.PromotedNoteID); err != nil {
			return domain.Candidate{}, nil, localError("scan candidate note", err)
		}
		note.CreatedAt = time.Unix(0, createdAt).UTC()
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return domain.Candidate{}, nil, localError("iterate candidate notes", err)
	}
	return candidate, notes, nil
}

func (s *Store) Candidates(ctx context.Context) ([]domain.Candidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT candidate_id, provider, repository, number, state, base_sha, head_sha, merge_sha, updated_at
		FROM candidates ORDER BY updated_at DESC, candidate_id`)
	if err != nil {
		return nil, localError("list candidates", err)
	}
	defer rows.Close()
	var candidates []domain.Candidate
	for rows.Next() {
		var candidate domain.Candidate
		var updatedAt int64
		if err := rows.Scan(&candidate.ID, &candidate.Provider, &candidate.Repository, &candidate.Number, &candidate.State, &candidate.BaseSHA, &candidate.HeadSHA, &candidate.MergeSHA, &updatedAt); err != nil {
			return nil, localError("scan candidate", err)
		}
		candidate.UpdatedAt = time.Unix(0, updatedAt).UTC()
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, localError("iterate candidates", err)
	}
	return candidates, nil
}

func (s *Store) PromoteCandidate(ctx context.Context, key domain.WorkingSetKey, candidateID string) (domain.CandidatePromotionResult, error) {
	result := domain.CandidatePromotionResult{CandidateID: candidateID}
	err := s.withImmediate(ctx, func(conn *sql.Conn) error {
		if err := s.requireWorkingSet(ctx, conn, key); err != nil {
			return err
		}
		var state domain.CandidateState
		var mergeSHA string
		err := conn.QueryRowContext(ctx, "SELECT state, merge_sha FROM candidates WHERE candidate_id = ?", candidateID).Scan(&state, &mergeSHA)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("candidate %q does not exist", candidateID))
		}
		if err != nil {
			return localError("read candidate before promotion", err)
		}
		if state != domain.CandidateMerged {
			return domain.NewError(domain.CodeValidation, errors.New("only merged candidates can be promoted"))
		}
		if mergeSHA != key.SourceSHA {
			return domain.NewError(domain.CodeStaleWorkingSet, errors.New("candidate merge SHA does not match the current Git source"))
		}
		notes, err := listCandidateNotesByState(ctx, conn, candidateID, domain.CandidateNoteDraft)
		if err != nil {
			return err
		}
		if len(notes) == 0 {
			return nil
		}
		for _, candidateNote := range notes {
			entity, err := entityByKeyFresh(ctx, conn, key.WorktreeID, key.SourceSHA, candidateNote.EntityKey)
			if domain.CodeOf(err) == domain.CodeEntityNotFound {
				if err := setCandidateNoteOutcome(ctx, conn, candidateID, candidateNote.ID, domain.CandidateNoteHistorical, ""); err != nil {
					return err
				}
				result.HistoricalNotes++
				continue
			}
			if err != nil {
				return err
			}
			noteID, err := newID()
			if err != nil {
				return localError("generate promoted note ID", err)
			}
			revisionID, err := newID()
			if err != nil {
				return localError("generate promoted note revision ID", err)
			}
			bindingState := domain.NoteBindingActive
			reviewReason := ""
			if entity.StructuralHash != candidateNote.StructuralHash {
				bindingState = domain.NoteBindingNeedsReview
				reviewReason = "candidate_structural_change"
				result.NeedsReviewNotes++
			} else {
				result.ActiveNotes++
			}
			pending := domain.Note{ID: noteID, RevisionID: revisionID, EntityKey: candidateNote.EntityKey, Kind: candidateNote.Kind, Body: candidateNote.Body, Author: candidateNote.Author, Origin: candidateNote.Origin, CreatedAt: time.Now().UTC(), BindingState: bindingState, BindingSourceSHA: key.SourceSHA, ReviewReason: reviewReason, Pending: true}
			if err := upsertPendingNote(ctx, conn, key.WorktreeID, pending); err != nil {
				return err
			}
			if err := setCandidateNoteOutcome(ctx, conn, candidateID, candidateNote.ID, domain.CandidateNotePromoted, noteID); err != nil {
				return err
			}
		}
		result.Promoted = true
		if result.ActiveNotes+result.NeedsReviewNotes == 0 {
			return nil
		}
		return s.rebuildSearch(ctx, conn, key)
	})
	if err != nil {
		return domain.CandidatePromotionResult{}, err
	}
	return result, nil
}

func listCandidateNotesByState(ctx context.Context, conn *sql.Conn, candidateID string, state domain.CandidateNoteState) ([]domain.CandidateNote, error) {
	rows, err := conn.QueryContext(ctx, `SELECT candidate_id, note_id, entity_key, structural_hash, kind, body, author, origin, created_at, state, promoted_note_id
		FROM candidate_notes WHERE candidate_id = ? AND state = ? ORDER BY created_at, note_id`, candidateID, state)
	if err != nil {
		return nil, localError("list promotable candidate notes", err)
	}
	defer rows.Close()
	var notes []domain.CandidateNote
	for rows.Next() {
		var note domain.CandidateNote
		var createdAt int64
		if err := rows.Scan(&note.CandidateID, &note.ID, &note.EntityKey, &note.StructuralHash, &note.Kind, &note.Body, &note.Author, &note.Origin, &createdAt, &note.State, &note.PromotedNoteID); err != nil {
			return nil, localError("scan promotable candidate note", err)
		}
		note.CreatedAt = time.Unix(0, createdAt).UTC()
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return nil, localError("iterate promotable candidate notes", err)
	}
	return notes, nil
}

func setCandidateNoteOutcome(ctx context.Context, conn *sql.Conn, candidateID, noteID string, state domain.CandidateNoteState, promotedNoteID string) error {
	result, err := conn.ExecContext(ctx, "UPDATE candidate_notes SET state = ?, promoted_note_id = ? WHERE candidate_id = ? AND note_id = ? AND state = ?", state, promotedNoteID, candidateID, noteID, domain.CandidateNoteDraft)
	if err != nil {
		return localError("record candidate note promotion outcome", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return localError("check candidate note promotion outcome", err)
	}
	if changed != 1 {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("candidate note changed while promoting"))
	}
	return nil
}

func validateCandidateInput(candidate domain.Candidate, notes []domain.CandidateNote) error {
	if candidate.ID == "" || candidate.Provider == "" || candidate.Repository == "" || candidate.Number < 1 || !domain.ValidCandidateState(candidate.State) || candidate.BaseSHA == "" || candidate.HeadSHA == "" || candidate.UpdatedAt.IsZero() {
		return domain.NewError(domain.CodeValidation, errors.New("candidate snapshot is incomplete or invalid"))
	}
	for _, note := range notes {
		if note.CandidateID != candidate.ID || note.ID == "" || note.EntityKey == "" || note.StructuralHash == "" || !domain.ValidNoteKind(note.Kind) || note.Body == "" || note.Author == "" || note.Origin == "" || note.CreatedAt.IsZero() || note.State != domain.CandidateNoteDraft || note.PromotedNoteID != "" {
			return domain.NewError(domain.CodeValidation, errors.New("candidate note is incomplete or invalid"))
		}
	}
	return nil
}

func candidatePayloadHash(candidate domain.Candidate, notes []domain.CandidateNote) (string, error) {
	contents, err := json.Marshal(struct {
		Candidate domain.Candidate       `json:"candidate"`
		Notes     []domain.CandidateNote `json:"notes"`
	}{Candidate: candidate, Notes: notes})
	if err != nil {
		return "", localError("serialize candidate snapshot", err)
	}
	digest := blake3.Sum256(contents)
	return fmt.Sprintf("%x", digest[:]), nil
}

func upsertCandidate(ctx context.Context, conn *sql.Conn, candidate domain.Candidate, payloadHash string) error {
	if _, err := conn.ExecContext(ctx, `INSERT INTO candidates (candidate_id, provider, repository, number, state, base_sha, head_sha, merge_sha, updated_at, payload_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(candidate_id) DO UPDATE SET provider = excluded.provider, repository = excluded.repository, number = excluded.number, state = excluded.state, base_sha = excluded.base_sha, head_sha = excluded.head_sha, merge_sha = excluded.merge_sha, updated_at = excluded.updated_at, payload_hash = excluded.payload_hash`, candidate.ID, candidate.Provider, candidate.Repository, candidate.Number, candidate.State, candidate.BaseSHA, candidate.HeadSHA, candidate.MergeSHA, candidate.UpdatedAt.UnixNano(), payloadHash); err != nil {
		return localError("write candidate snapshot", err)
	}
	return nil
}

func insertCandidateNotes(ctx context.Context, conn *sql.Conn, notes []domain.CandidateNote) error {
	for _, note := range notes {
		if _, err := conn.ExecContext(ctx, `INSERT INTO candidate_notes (candidate_id, note_id, entity_key, structural_hash, kind, body, author, origin, created_at, state, promoted_note_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')
			ON CONFLICT(candidate_id, note_id) DO NOTHING`, note.CandidateID, note.ID, note.EntityKey, note.StructuralHash, note.Kind, note.Body, note.Author, note.Origin, note.CreatedAt.UnixNano(), note.State); err != nil {
			return localError("write candidate note", err)
		}
	}
	return nil
}
