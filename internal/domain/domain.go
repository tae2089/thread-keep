package domain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ErrorCode string

const (
	CodeValidation          ErrorCode = "validation"
	CodeRepositoryState     ErrorCode = "repository_state"
	CodeNotInitialized      ErrorCode = "not_initialized"
	CodeEntityNotFound      ErrorCode = "entity_not_found"
	CodeNothingToCommit     ErrorCode = "nothing_to_commit"
	CodeStaleWorkingSet     ErrorCode = "stale_working_set"
	CodeWorkingSetDirty     ErrorCode = "working_set_dirty"
	CodeConcurrentUpdate    ErrorCode = "concurrent_update"
	CodeCoverageIncomplete  ErrorCode = "coverage_incomplete"
	CodeLocalStorage        ErrorCode = "local_storage"
	CodeBusy                ErrorCode = "busy"
	CodeRemoteConflict      ErrorCode = "remote_conflict"
	CodeAuth                ErrorCode = "auth"
	CodeIncompatiblePayload ErrorCode = "incompatible_payload"
)

type Error struct {
	Code ErrorCode
	Err  error
}

type EntityKind string

const (
	EntityFunction  EntityKind = "function"
	EntityMethod    EntityKind = "method"
	EntityType      EntityKind = "type"
	EntityClass     EntityKind = "class"
	EntityInterface EntityKind = "interface"
	EntityEnum      EntityKind = "enum"
)

type Entity struct {
	Language       string     `json:"language"`
	Key            string     `json:"key"`
	Kind           EntityKind `json:"kind"`
	Name           string     `json:"name"`
	Signature      string     `json:"signature"`
	Path           string     `json:"path"`
	StartLine      int        `json:"start_line"`
	EndLine        int        `json:"end_line"`
	SourceSHA      string     `json:"source_sha"`
	StructuralHash string     `json:"structural_hash"`
}

type CoverageState string

const (
	CoverageIndexed     CoverageState = "indexed"
	CoverageMissingPack CoverageState = "missing_pack"
	CoverageFailed      CoverageState = "failed"
)

type Coverage struct {
	Language       string        `json:"language"`
	State          CoverageState `json:"state"`
	IndexerID      string        `json:"indexer_id"`
	IndexerVersion string        `json:"indexer_version"`
	SourceSHA      string        `json:"source_sha"`
	Detail         string        `json:"detail,omitempty"`
}

type LanguageProjection struct {
	Coverage Coverage `json:"coverage"`
	Entities []Entity `json:"entities"`
}

type IndexerState string

const (
	IndexerBuiltin   IndexerState = "builtin"
	IndexerInstalled IndexerState = "installed"
	IndexerMissing   IndexerState = "missing"
)

type IndexerStatus struct {
	Language string       `json:"language"`
	PackID   string       `json:"pack_id"`
	State    IndexerState `json:"state"`
	Detected bool         `json:"detected"`
	Path     string       `json:"path,omitempty"`
}

type NoteKind string

const (
	NoteIntent     NoteKind = "intent"
	NoteDecision   NoteKind = "decision"
	NoteConstraint NoteKind = "constraint"
	NoteExample    NoteKind = "example"
	NoteWarning    NoteKind = "warning"
)

type NoteBindingState string

const (
	NoteBindingActive      NoteBindingState = "active"
	NoteBindingNeedsReview NoteBindingState = "needs_review"
	NoteBindingHistorical  NoteBindingState = "historical"
)

type Note struct {
	ID                   string           `json:"id"`
	RevisionID           string           `json:"revision_id,omitempty"`
	SupersedesRevisionID string           `json:"supersedes_revision_id,omitempty"`
	EntityKey            string           `json:"entity_key"`
	Kind                 NoteKind         `json:"kind"`
	Body                 string           `json:"body"`
	Author               string           `json:"author"`
	Origin               string           `json:"origin"`
	CreatedAt            time.Time        `json:"created_at"`
	BindingState         NoteBindingState `json:"binding_state,omitempty"`
	BindingSourceSHA     string           `json:"binding_source_sha,omitempty"`
	ReviewReason         string           `json:"review_reason,omitempty"`
	Topics               []string         `json:"topics,omitempty"`
	Pending              bool             `json:"pending"`
}

type WorkingSetKey struct {
	RepositoryID string `json:"repository_id"`
	WorktreeID   string `json:"worktree_id"`
	RefName      string `json:"ref_name"`
	SourceSHA    string `json:"source_sha"`
}

type ContextCommit struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`
	RefName   string    `json:"ref_name"`
	SourceSHA string    `json:"source_sha"`
	Message   string    `json:"message"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

type ContextSnapshotProvenance struct {
	Language       string `json:"language"`
	IndexerID      string `json:"indexer_id"`
	IndexerVersion string `json:"indexer_version"`
	SourceSHA      string `json:"source_sha"`
}

type ContextRevisionMapping struct {
	EntityKey        string           `json:"entity_key"`
	NoteID           string           `json:"note_id"`
	RevisionID       string           `json:"revision_id"`
	BindingState     NoteBindingState `json:"binding_state"`
	BindingSourceSHA string           `json:"binding_source_sha"`
	ReviewReason     string           `json:"review_reason,omitempty"`
}

type ContextObject struct {
	SchemaVersion    int                         `json:"schema_version"`
	RepositoryID     string                      `json:"repository_id"`
	RefName          string                      `json:"ref_name"`
	ParentID         string                      `json:"parent_id,omitempty"`
	ParentIDs        []string                    `json:"parent_ids,omitempty"`
	LegacyParentID   string                      `json:"legacy_parent_id,omitempty"`
	SourceSHA        string                      `json:"source_sha"`
	Message          string                      `json:"message"`
	Author           string                      `json:"author"`
	CreatedAt        time.Time                   `json:"created_at"`
	Provenance       []ContextSnapshotProvenance `json:"provenance,omitempty"`
	Entities         []Entity                    `json:"entities"`
	Notes            []Note                      `json:"notes"`
	RevisionMappings []ContextRevisionMapping    `json:"revision_mappings,omitempty"`
	LandingReceipts  []LandingReceipt            `json:"landing_receipts,omitempty"`
}

type Remote struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type RemoteRef struct {
	RemoteName string `json:"remote_name"`
	RefName    string `json:"ref_name"`
	CommitID   string `json:"commit_id,omitempty"`
	SourceSHA  string `json:"source_sha,omitempty"`
	Version    int    `json:"version"`
}

type ContextRef struct {
	RefName   string `json:"ref_name"`
	CommitID  string `json:"commit_id,omitempty"`
	SourceSHA string `json:"source_sha,omitempty"`
	Version   int    `json:"version"`
}

type RemoteSyncResult struct {
	RemoteName         string `json:"remote_name"`
	RefName            string `json:"ref_name"`
	LocalTip           string `json:"local_tip,omitempty"`
	RemoteTip          string `json:"remote_tip,omitempty"`
	TrackingTip        string `json:"tracking_tip,omitempty"`
	Outcome            string `json:"outcome"`
	TransferredObjects int    `json:"transferred_objects"`
}

type SearchHit struct {
	EntityKey     string             `json:"entity_key"`
	Name          string             `json:"name"`
	Path          string             `json:"path"`
	Pending       bool               `json:"pending"`
	Snippet       string             `json:"snippet"`
	MatchedFields []SearchMatchField `json:"matched_fields"`
	MatchedTerms  []string           `json:"matched_terms"`
	NoteIDs       []string           `json:"note_ids,omitempty"`
	BindingState  NoteBindingState   `json:"binding_state,omitempty"`
	Fresh         bool               `json:"fresh"`
}

type SearchMatchField string

const (
	SearchMatchEntityKey SearchMatchField = "entity_key"
	SearchMatchName      SearchMatchField = "name"
	SearchMatchSignature SearchMatchField = "signature"
	SearchMatchPath      SearchMatchField = "path"
	SearchMatchNoteBody  SearchMatchField = "note_body"
)

type RelatedEntity struct {
	EntityKey string `json:"entity_key"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	EdgeKind  string `json:"edge_kind"`
	Fresh     bool   `json:"fresh"`
}

type Status struct {
	RepositoryID     string     `json:"repository_id"`
	WorktreeID       string     `json:"worktree_id"`
	RefName          string     `json:"ref_name"`
	SourceSHA        string     `json:"source_sha"`
	EntityCount      int        `json:"entity_count"`
	PendingNotes     int        `json:"pending_notes"`
	ContextCommitID  string     `json:"context_commit_id"`
	CoverageComplete bool       `json:"coverage_complete"`
	Coverage         []Coverage `json:"coverage"`
}

func (e *Error) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func NewError(code ErrorCode, err error) error {
	return &Error{Code: code, Err: err}
}

func CodeOf(err error) ErrorCode {
	var typed *Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

func ValidNoteKind(kind NoteKind) bool {
	switch kind {
	case NoteIntent, NoteDecision, NoteConstraint, NoteExample, NoteWarning:
		return true
	default:
		return false
	}
}

func ValidNoteBindingState(state NoteBindingState) bool {
	switch state {
	case NoteBindingActive, NoteBindingNeedsReview, NoteBindingHistorical:
		return true
	default:
		return false
	}
}

func NormalizeNoteTopics(values []string) ([]string, error) {
	if len(values) > 16 {
		return nil, NewError(CodeValidation, errors.New("a note may contain at most 16 topics"))
	}
	topics := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || len([]rune(value)) > 64 {
			return nil, NewError(CodeValidation, errors.New("each note topic must contain 1 to 64 characters"))
		}
		if _, found := seen[value]; found {
			return nil, NewError(CodeValidation, fmt.Errorf("duplicate note topic %q", value))
		}
		seen[value] = struct{}{}
		topics = append(topics, value)
	}
	sort.Strings(topics)
	return topics, nil
}

func NormalizeLegacyNote(note Note, sourceSHA string) Note {
	if note.RevisionID == "" {
		note.RevisionID = note.ID
	}
	if note.BindingState == "" {
		note.BindingState = NoteBindingActive
	}
	if note.BindingSourceSHA == "" {
		note.BindingSourceSHA = sourceSHA
	}
	return note
}

func NormalizeContextCommitID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return "", NewError(CodeValidation, errors.New("context commit ID must be 64 hexadecimal characters"))
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", NewError(CodeValidation, errors.New("context commit ID must be 64 hexadecimal characters"))
	}
	return strings.ToLower(value), nil
}

func NormalizeRemoteName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 64 {
		return "", NewError(CodeValidation, errors.New("remote name must contain 1 to 64 letters, digits, dots, underscores or hyphens"))
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			if index == 0 && (character == '.' || character == '-' || character == '_') {
				return "", NewError(CodeValidation, errors.New("remote name must begin with a letter or digit"))
			}
			continue
		}
		return "", NewError(CodeValidation, errors.New("remote name must contain only letters, digits, dots, underscores or hyphens"))
	}
	return value, nil
}
