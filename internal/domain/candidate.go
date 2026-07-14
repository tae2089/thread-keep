package domain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

const MaxCandidateEnvelopeBytes = 1 << 20

const maxCandidateNotes = 1000

type CandidateState string

const (
	CandidateOpen   CandidateState = "open"
	CandidateMerged CandidateState = "merged"
	CandidateClosed CandidateState = "closed"
)

type CandidateNoteState string

const (
	CandidateNoteDraft      CandidateNoteState = "draft"
	CandidateNoteHistorical CandidateNoteState = "historical"
	CandidateNotePromoted   CandidateNoteState = "promoted"
)

type Candidate struct {
	ID         string         `json:"id"`
	Provider   string         `json:"provider"`
	Repository string         `json:"repository"`
	Number     int            `json:"number"`
	State      CandidateState `json:"state"`
	BaseSHA    string         `json:"base_sha"`
	HeadSHA    string         `json:"head_sha"`
	MergeSHA   string         `json:"merge_sha,omitempty"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

type CandidateNote struct {
	CandidateID    string             `json:"candidate_id"`
	ID             string             `json:"id"`
	EntityKey      string             `json:"entity_key"`
	StructuralHash string             `json:"structural_hash"`
	Kind           NoteKind           `json:"kind"`
	Body           string             `json:"body"`
	Author         string             `json:"author"`
	Origin         string             `json:"origin"`
	CreatedAt      time.Time          `json:"created_at"`
	State          CandidateNoteState `json:"state"`
	PromotedNoteID string             `json:"promoted_note_id,omitempty"`
}

type CandidatePromotionResult struct {
	CandidateID      string `json:"candidate_id"`
	Promoted         bool   `json:"promoted"`
	ActiveNotes      int    `json:"active_notes"`
	NeedsReviewNotes int    `json:"needs_review_notes"`
	HistoricalNotes  int    `json:"historical_notes"`
}

type candidateEnvelope struct {
	SchemaVersion int                     `json:"schema_version"`
	Provider      string                  `json:"provider"`
	Repository    string                  `json:"repository"`
	Number        int                     `json:"number"`
	State         CandidateState          `json:"state"`
	BaseSHA       string                  `json:"base_sha"`
	HeadSHA       string                  `json:"head_sha"`
	MergeSHA      string                  `json:"merge_sha"`
	UpdatedAt     time.Time               `json:"updated_at"`
	Notes         []candidateEnvelopeNote `json:"notes"`
}

type candidateEnvelopeNote struct {
	ID             string    `json:"id"`
	EntityKey      string    `json:"entity_key"`
	StructuralHash string    `json:"structural_hash"`
	Kind           NoteKind  `json:"kind"`
	Body           string    `json:"body"`
	Author         string    `json:"author"`
	Origin         string    `json:"origin"`
	CreatedAt      time.Time `json:"created_at"`
}

func DecodeCandidateEnvelope(contents []byte) (Candidate, []CandidateNote, error) {
	if len(contents) == 0 || len(contents) > MaxCandidateEnvelopeBytes {
		return Candidate{}, nil, NewError(CodeValidation, fmt.Errorf("candidate envelope must contain at most %d bytes", MaxCandidateEnvelopeBytes))
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var envelope candidateEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return Candidate{}, nil, NewError(CodeValidation, fmt.Errorf("decode candidate envelope: %w", err))
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Candidate{}, nil, NewError(CodeValidation, errors.New("candidate envelope must contain one JSON value"))
	}
	return normalizeCandidateEnvelope(envelope)
}

func normalizeCandidateEnvelope(envelope candidateEnvelope) (Candidate, []CandidateNote, error) {
	if envelope.SchemaVersion != 1 {
		return Candidate{}, nil, NewError(CodeValidation, fmt.Errorf("unsupported candidate envelope schema version %d", envelope.SchemaVersion))
	}
	provider, err := normalizeCandidateProvider(envelope.Provider)
	if err != nil {
		return Candidate{}, nil, err
	}
	repository, err := normalizeCandidateRepository(envelope.Repository)
	if err != nil {
		return Candidate{}, nil, err
	}
	if envelope.Number < 1 {
		return Candidate{}, nil, NewError(CodeValidation, errors.New("candidate number must be positive"))
	}
	if !ValidCandidateState(envelope.State) || envelope.UpdatedAt.IsZero() || !validCandidateSHA(envelope.BaseSHA) || !validCandidateSHA(envelope.HeadSHA) {
		return Candidate{}, nil, NewError(CodeValidation, errors.New("candidate snapshot is incomplete or invalid"))
	}
	if envelope.State == CandidateMerged {
		if !validCandidateSHA(envelope.MergeSHA) {
			return Candidate{}, nil, NewError(CodeValidation, errors.New("merged candidate requires a valid merge SHA"))
		}
	} else if envelope.MergeSHA != "" {
		return Candidate{}, nil, NewError(CodeValidation, errors.New("open candidate or closed candidate must not include a merge SHA"))
	}
	if len(envelope.Notes) > maxCandidateNotes {
		return Candidate{}, nil, NewError(CodeValidation, fmt.Errorf("candidate envelope exceeds %d notes", maxCandidateNotes))
	}
	candidate := Candidate{ID: provider + ":" + repository + "#" + fmt.Sprint(envelope.Number), Provider: provider, Repository: repository, Number: envelope.Number, State: envelope.State, BaseSHA: strings.ToLower(envelope.BaseSHA), HeadSHA: strings.ToLower(envelope.HeadSHA), MergeSHA: strings.ToLower(envelope.MergeSHA), UpdatedAt: envelope.UpdatedAt.UTC()}
	noteIDs := make(map[string]struct{}, len(envelope.Notes))
	notes := make([]CandidateNote, 0, len(envelope.Notes))
	for _, rawNote := range envelope.Notes {
		note := CandidateNote{ID: rawNote.ID, EntityKey: rawNote.EntityKey, StructuralHash: rawNote.StructuralHash, Kind: rawNote.Kind, Body: rawNote.Body, Author: rawNote.Author, Origin: rawNote.Origin, CreatedAt: rawNote.CreatedAt}
		if err := validateCandidateNote(note); err != nil {
			return Candidate{}, nil, err
		}
		if _, found := noteIDs[note.ID]; found {
			return Candidate{}, nil, NewError(CodeValidation, fmt.Errorf("candidate envelope contains duplicate note ID %q", note.ID))
		}
		noteIDs[note.ID] = struct{}{}
		note.CandidateID = candidate.ID
		note.StructuralHash = strings.ToLower(note.StructuralHash)
		note.CreatedAt = note.CreatedAt.UTC()
		note.State = CandidateNoteDraft
		note.PromotedNoteID = ""
		notes = append(notes, note)
	}
	return candidate, notes, nil
}

func ValidCandidateState(state CandidateState) bool {
	return state == CandidateOpen || state == CandidateMerged || state == CandidateClosed
}

func normalizeCandidateProvider(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 0 || len(value) > 64 {
		return "", NewError(CodeValidation, errors.New("candidate provider must contain 1 to 64 identifier characters"))
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			if index == 0 && (character == '.' || character == '_' || character == '-') {
				return "", NewError(CodeValidation, errors.New("candidate provider must begin with a letter or digit"))
			}
			continue
		}
		return "", NewError(CodeValidation, errors.New("candidate provider contains an invalid character"))
	}
	return value, nil
}

func normalizeCandidateRepository(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 256 || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.ContainsAny(value, "\\:#\t\r\n ") {
		return "", NewError(CodeValidation, errors.New("candidate repository is invalid"))
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", NewError(CodeValidation, errors.New("candidate repository is invalid"))
		}
	}
	return value, nil
}

func validCandidateSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateCandidateNote(note CandidateNote) error {
	if len(note.ID) == 0 || len(note.ID) > 128 || strings.IndexFunc(note.ID, unicode.IsSpace) >= 0 || strings.TrimSpace(note.EntityKey) == "" || !validCandidateSHA(note.StructuralHash) || !ValidNoteKind(note.Kind) || strings.TrimSpace(note.Body) == "" || len(note.Body) > 64*1024 || strings.TrimSpace(note.Author) == "" || strings.TrimSpace(note.Origin) == "" || note.CreatedAt.IsZero() {
		return NewError(CodeValidation, errors.New("candidate note is incomplete or invalid"))
	}
	return nil
}
