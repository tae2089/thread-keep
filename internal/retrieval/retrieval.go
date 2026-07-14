package retrieval

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

type Candidate struct {
	Root          domain.Entity
	Bound         domain.Entity
	Note          domain.Note
	Reasons       []domain.SelectionReason
	Historical    bool
	ContextCommit string
}

type Input struct {
	Query       domain.ContextQuery
	Source      domain.RetrievalSource
	Anchor      domain.ResolvedAnchor
	Candidates  []Candidate
	Complete    bool
	Diagnostics []domain.RetrievalDiagnostic
}

func Assemble(input Input) (domain.ContextBundle, error) {
	query, err := NormalizeQuery(input.Query)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	items := make([]domain.ContextItem, 0, len(input.Candidates))
	seen := make(map[string]struct{}, len(input.Candidates))
	for _, candidate := range input.Candidates {
		if !matches(query, candidate) {
			continue
		}
		item := domain.ContextItem{
			Note:          candidate.Note,
			BoundEntity:   entityRef(candidate.Bound),
			RootEntity:    entityRef(candidate.Root),
			Reasons:       append([]domain.SelectionReason(nil), candidate.Reasons...),
			Historical:    candidate.Historical,
			ContextCommit: candidate.ContextCommit,
		}
		key := itemKey(item)
		if _, found := seen[key]; found {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, item)
	}
	sort.Slice(items, func(left, right int) bool {
		return lessItem(items[left], items[right])
	})
	diagnostics := append([]domain.RetrievalDiagnostic(nil), input.Diagnostics...)
	complete := input.Complete
	if len(items) > query.Limit {
		items = items[:query.Limit]
		diagnostics = append(diagnostics, domain.RetrievalDiagnostic{Code: "results_truncated", Detail: fmt.Sprintf("limited to %d items", query.Limit)})
		complete = false
	}
	if items == nil {
		items = []domain.ContextItem{}
	}
	return domain.ContextBundle{
		Source:      input.Source,
		Anchor:      input.Anchor,
		Complete:    complete,
		Diagnostics: diagnostics,
		Items:       items,
	}, nil
}

func NormalizeQuery(query domain.ContextQuery) (domain.ContextQuery, error) {
	query.Anchor.EntityKey = strings.TrimSpace(query.Anchor.EntityKey)
	query.Anchor.Query = strings.TrimSpace(query.Anchor.Query)
	query.Anchor.BaseContextID = strings.TrimSpace(query.Anchor.BaseContextID)
	query.Text = strings.TrimSpace(query.Text)
	switch query.Anchor.Kind {
	case domain.AnchorEntity:
		if query.Anchor.EntityKey == "" || query.Anchor.Query != "" || query.Anchor.BaseContextID != "" {
			return domain.ContextQuery{}, validationError("entity anchor requires only an entity key")
		}
	case domain.AnchorText:
		if query.Anchor.Query == "" || query.Anchor.EntityKey != "" || query.Anchor.BaseContextID != "" || query.Text != "" {
			return domain.ContextQuery{}, validationError("text anchor requires only a query")
		}
	case domain.AnchorChange:
		if query.Anchor.EntityKey != "" || query.Anchor.Query != "" {
			return domain.ContextQuery{}, validationError("change anchor accepts only an optional base context ID")
		}
	default:
		return domain.ContextQuery{}, validationError("context anchor must be entity or text")
	}
	if query.History == "" {
		query.History = domain.HistoryCurrent
	}
	if query.History != domain.HistoryCurrent && query.History != domain.HistoryAll {
		return domain.ContextQuery{}, validationError("history mode must be current or all")
	}
	if query.Limit == 0 {
		query.Limit = defaultLimit
	}
	if query.Limit < 1 || query.Limit > maxLimit {
		return domain.ContextQuery{}, validationError(fmt.Sprintf("context limit must be between 1 and %d", maxLimit))
	}
	if len(query.States) == 0 {
		query.States = []domain.NoteBindingState{domain.NoteBindingActive}
	}
	for _, kind := range query.Kinds {
		if !domain.ValidNoteKind(kind) {
			return domain.ContextQuery{}, validationError(fmt.Sprintf("invalid note kind %q", kind))
		}
	}
	for _, state := range query.States {
		if !domain.ValidNoteBindingState(state) {
			return domain.ContextQuery{}, validationError(fmt.Sprintf("invalid note binding state %q", state))
		}
	}
	if query.Topics != nil {
		topics, err := domain.NormalizeNoteTopics(query.Topics)
		if err != nil {
			return domain.ContextQuery{}, err
		}
		query.Topics = topics
	}
	return query, nil
}

func matches(query domain.ContextQuery, candidate Candidate) bool {
	if candidate.Historical && query.History != domain.HistoryAll {
		return false
	}
	if !containsState(query.States, candidate.Note.BindingState) {
		return false
	}
	if len(query.Kinds) != 0 && !containsKind(query.Kinds, candidate.Note.Kind) {
		return false
	}
	if len(query.Languages) != 0 && !containsString(query.Languages, candidate.Bound.Language) {
		return false
	}
	if len(query.EntityKinds) != 0 && !containsEntityKind(query.EntityKinds, candidate.Bound.Kind) {
		return false
	}
	if len(query.Paths) != 0 && !hasPathPrefix(query.Paths, candidate.Bound.Path) {
		return false
	}
	if query.Text != "" && !matchesText(query.Text, candidate) {
		return false
	}
	if len(query.Topics) != 0 && !containsAllTopics(candidate.Note.Topics, query.Topics) {
		return false
	}
	return true
}

func containsAllTopics(noteTopics, requested []string) bool {
	for _, topic := range requested {
		found := false
		for _, candidate := range noteTopics {
			if candidate == topic {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func matchesText(query string, candidate Candidate) bool {
	values := []string{candidate.Bound.Key, candidate.Bound.Name, candidate.Bound.Signature, candidate.Bound.Path, candidate.Note.Body}
	for _, term := range strings.Fields(strings.ToLower(query)) {
		found := false
		for _, value := range values {
			if strings.Contains(strings.ToLower(value), term) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsState(states []domain.NoteBindingState, value domain.NoteBindingState) bool {
	for _, state := range states {
		if state == value {
			return true
		}
	}
	return false
}

func containsKind(kinds []domain.NoteKind, value domain.NoteKind) bool {
	for _, kind := range kinds {
		if kind == value {
			return true
		}
	}
	return false
}

func containsEntityKind(kinds []domain.EntityKind, value domain.EntityKind) bool {
	for _, kind := range kinds {
		if kind == value {
			return true
		}
	}
	return false
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func hasPathPrefix(prefixes []string, value string) bool {
	for _, prefix := range prefixes {
		prefix = strings.Trim(strings.TrimSpace(prefix), "/")
		if prefix != "" && (value == prefix || strings.HasPrefix(value, prefix+"/")) {
			return true
		}
	}
	return false
}

func entityRef(entity domain.Entity) domain.EntityRef {
	return domain.EntityRef{Language: entity.Language, EntityKey: entity.Key, Kind: entity.Kind, Name: entity.Name, Path: entity.Path, SourceSHA: entity.SourceSHA}
}

func itemKey(item domain.ContextItem) string {
	return strings.Join([]string{item.RootEntity.EntityKey, item.BoundEntity.EntityKey, item.Note.ID, item.Note.RevisionID, string(item.Note.BindingState), item.Note.BindingSourceSHA, fmt.Sprint(item.Historical), item.ContextCommit}, "\x00")
}

func lessItem(left, right domain.ContextItem) bool {
	if left.Historical != right.Historical {
		return !left.Historical
	}
	leftReason := reasonPriority(left.Reasons)
	rightReason := reasonPriority(right.Reasons)
	if leftReason != rightReason {
		return leftReason < rightReason
	}
	leftKind := noteKindPriority(left.Note.Kind)
	rightKind := noteKindPriority(right.Note.Kind)
	if leftKind != rightKind {
		return leftKind < rightKind
	}
	if left.BoundEntity.EntityKey != right.BoundEntity.EntityKey {
		return left.BoundEntity.EntityKey < right.BoundEntity.EntityKey
	}
	if left.Note.ID != right.Note.ID {
		return left.Note.ID < right.Note.ID
	}
	return left.Note.RevisionID < right.Note.RevisionID
}

func reasonPriority(reasons []domain.SelectionReason) int {
	priority := 3
	for _, reason := range reasons {
		current := 2
		switch reason.Kind {
		case domain.ReasonDirectEntity, domain.ReasonTextEntityField, domain.ReasonTextNoteBody:
			current = 0
		}
		if current < priority {
			priority = current
		}
	}
	return priority
}

func noteKindPriority(kind domain.NoteKind) int {
	switch kind {
	case domain.NoteConstraint:
		return 0
	case domain.NoteWarning:
		return 1
	case domain.NoteDecision:
		return 2
	case domain.NoteIntent:
		return 3
	case domain.NoteExample:
		return 4
	default:
		return 5
	}
}

func validationError(message string) error {
	return domain.NewError(domain.CodeValidation, errors.New(message))
}
