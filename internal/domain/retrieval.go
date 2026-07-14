package domain

type AnchorKind string

const (
	AnchorChange AnchorKind = "change"
	AnchorEntity AnchorKind = "entity"
	AnchorText   AnchorKind = "text"
)

type HistoryMode string

const (
	HistoryCurrent HistoryMode = "current"
	HistoryAll     HistoryMode = "all"
)

type EntityChangeKind string

const (
	ChangeAdded    EntityChangeKind = "added"
	ChangeModified EntityChangeKind = "modified"
	ChangeMoved    EntityChangeKind = "moved"
	ChangeRemoved  EntityChangeKind = "removed"
)

type SelectionReasonKind string

const (
	ReasonChangedEntity      SelectionReasonKind = "changed_entity"
	ReasonDirectEntity       SelectionReasonKind = "direct_entity"
	ReasonTextEntityField    SelectionReasonKind = "text_entity_field"
	ReasonTextNoteBody       SelectionReasonKind = "text_note_body"
	ReasonTopicExact         SelectionReasonKind = "topic_exact"
	ReasonHistoricalRevision SelectionReasonKind = "historical_revision"
)

type ContextAnchor struct {
	Kind          AnchorKind `json:"kind"`
	EntityKey     string     `json:"entity_key,omitempty"`
	Query         string     `json:"query,omitempty"`
	BaseContextID string     `json:"base_context_id,omitempty"`
}

type ContextQuery struct {
	Anchor      ContextAnchor      `json:"anchor"`
	Text        string             `json:"text,omitempty"`
	Kinds       []NoteKind         `json:"kinds,omitempty"`
	States      []NoteBindingState `json:"states,omitempty"`
	Topics      []string           `json:"topics,omitempty"`
	Languages   []string           `json:"languages,omitempty"`
	Paths       []string           `json:"paths,omitempty"`
	EntityKinds []EntityKind       `json:"entity_kinds,omitempty"`
	History     HistoryMode        `json:"history,omitempty"`
	Limit       int                `json:"limit,omitempty"`
}

type RetrievalSource struct {
	SourceSHA       string `json:"source_sha"`
	ContextCommitID string `json:"context_commit_id,omitempty"`
	BaseSourceSHA   string `json:"base_source_sha,omitempty"`
}

type ResolvedAnchor struct {
	Kind       AnchorKind     `json:"kind"`
	EntityKeys []string       `json:"entity_keys,omitempty"`
	Query      string         `json:"query,omitempty"`
	Changes    []EntityChange `json:"changes,omitempty"`
}

type EntityChange struct {
	Kind   EntityChangeKind `json:"kind"`
	Base   Entity           `json:"base,omitempty"`
	Target Entity           `json:"target,omitempty"`
}

type EntityRef struct {
	Language  string     `json:"language"`
	EntityKey string     `json:"entity_key"`
	Kind      EntityKind `json:"kind"`
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	SourceSHA string     `json:"source_sha"`
}

type SelectionReason struct {
	Kind         SelectionReasonKind `json:"kind"`
	MatchedField SearchMatchField    `json:"matched_field,omitempty"`
	MatchedTerms []string            `json:"matched_terms,omitempty"`
	ChangeKind   EntityChangeKind    `json:"change_kind,omitempty"`
}

type RetrievalDiagnostic struct {
	Code     string `json:"code"`
	Detail   string `json:"detail,omitempty"`
	Language string `json:"language,omitempty"`
}

type ContextItem struct {
	Note          Note              `json:"note"`
	BoundEntity   EntityRef         `json:"bound_entity"`
	RootEntity    EntityRef         `json:"root_entity"`
	Reasons       []SelectionReason `json:"reasons"`
	Historical    bool              `json:"historical"`
	ContextCommit string            `json:"context_commit_id,omitempty"`
}

type ContextBundle struct {
	Source      RetrievalSource       `json:"source"`
	Anchor      ResolvedAnchor        `json:"anchor"`
	Complete    bool                  `json:"complete"`
	Diagnostics []RetrievalDiagnostic `json:"diagnostics,omitempty"`
	Items       []ContextItem         `json:"items"`
}
