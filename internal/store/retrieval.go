package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/retrieval"
)

func (s *Store) AssembleContext(ctx context.Context, key domain.WorkingSetKey, query domain.ContextQuery) (domain.ContextBundle, error) {
	normalized, err := retrieval.NormalizeQuery(query)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return domain.ContextBundle{}, localError("open SQLite connection", err)
	}
	defer conn.Close()
	if err := s.requireWorkingSet(ctx, conn, key); err != nil {
		return domain.ContextBundle{}, err
	}
	entities, err := listFreshEntities(ctx, conn, key.WorktreeID, key.SourceSHA)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	notes, parentID, err := currentNotes(ctx, s, conn, key)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	var roots []domain.Entity
	var reasons map[string][]domain.SelectionReason
	var changes []domain.EntityChange
	source := domain.RetrievalSource{SourceSHA: key.SourceSHA, ContextCommitID: parentID}
	var diagnostics []domain.RetrievalDiagnostic
	if normalized.Anchor.Kind == domain.AnchorChange {
		baseID := normalized.Anchor.BaseContextID
		if baseID == "" {
			baseID = parentID
		}
		if baseID == "" {
			return domain.ContextBundle{}, domain.NewError(domain.CodeValidation, errors.New("change context requires --base-context when no context tip exists"))
		}
		baseID, err = domain.NormalizeContextCommitID(baseID)
		if err != nil {
			return domain.ContextBundle{}, err
		}
		if normalized.Anchor.BaseContextID != "" && parentID != "" {
			reachable, err := s.IsAncestor(parentID, baseID, key.RepositoryID, key.RefName)
			if err != nil {
				return domain.ContextBundle{}, err
			}
			if !reachable {
				return domain.ContextBundle{}, domain.NewError(domain.CodeValidation, errors.New("base context snapshot is not reachable from the current context tip"))
			}
		}
		base, err := s.ReadContextObject(baseID, key.RepositoryID, key.RefName)
		if err != nil {
			return domain.ContextBundle{}, err
		}
		changes = retrieval.ClassifyChanges(base.Entities, entities)
		roots, reasons = changeRoots(changes)
		source.ContextCommitID = baseID
		source.BaseSourceSHA = base.SourceSHA
		if base.SchemaVersion < 3 {
			diagnostics = append(diagnostics, domain.RetrievalDiagnostic{Code: "base_coverage_unknown", Detail: "legacy base snapshot does not record complete indexer provenance"})
		}
	} else {
		roots, reasons, err = assemblyRoots(ctx, conn, key, normalized, entities, notes)
		if err != nil {
			return domain.ContextBundle{}, err
		}
	}
	candidates := candidatesForRoots(roots, reasons, notes)
	if normalized.History == domain.HistoryAll && parentID != "" {
		historical, err := s.historicalCandidates(parentID, key, normalized, roots, reasons, changes, notes)
		if err != nil {
			return domain.ContextBundle{}, err
		}
		candidates = append(candidates, historical...)
	}
	coverage, err := listCoverage(ctx, conn, key.WorktreeID)
	if err != nil {
		return domain.ContextBundle{}, err
	}
	entityKeys := make([]string, 0, len(roots))
	for _, root := range roots {
		entityKeys = append(entityKeys, root.Key)
	}
	resolved := domain.ResolvedAnchor{Kind: normalized.Anchor.Kind, EntityKeys: entityKeys, Query: normalized.Anchor.Query, Changes: changes}
	return retrieval.Assemble(retrieval.Input{
		Query:       normalized,
		Source:      source,
		Anchor:      resolved,
		Candidates:  candidates,
		Complete:    coverageComplete(coverage, key.SourceSHA) && len(diagnostics) == 0,
		Diagnostics: diagnostics,
	})
}

func changeRoots(changes []domain.EntityChange) ([]domain.Entity, map[string][]domain.SelectionReason) {
	roots := make([]domain.Entity, 0, len(changes))
	reasons := make(map[string][]domain.SelectionReason, len(changes))
	for _, change := range changes {
		root := change.Target
		if change.Kind == domain.ChangeRemoved {
			root = change.Base
		}
		roots = append(roots, root)
		reasons[root.Key] = []domain.SelectionReason{{Kind: domain.ReasonChangedEntity, ChangeKind: change.Kind}}
	}
	return roots, reasons
}

func currentNotes(ctx context.Context, store *Store, conn *sql.Conn, key domain.WorkingSetKey) ([]domain.Note, string, error) {
	parentID, _, err := store.ref(ctx, conn, key.RefName)
	if err != nil {
		return nil, "", err
	}
	var committed []domain.Note
	if parentID != "" {
		committed, err = listCommittedNotes(ctx, conn, parentID)
		if err != nil {
			return nil, "", err
		}
	}
	pending, err := listPendingNotes(ctx, conn, key.WorktreeID)
	if err != nil {
		return nil, "", err
	}
	return effectiveNotes(committed, pending), parentID, nil
}

func assemblyRoots(ctx context.Context, conn *sql.Conn, key domain.WorkingSetKey, query domain.ContextQuery, entities []domain.Entity, notes []domain.Note) ([]domain.Entity, map[string][]domain.SelectionReason, error) {
	if query.Anchor.Kind == domain.AnchorEntity {
		root, err := entityByKeyFresh(ctx, conn, key.WorktreeID, key.SourceSHA, query.Anchor.EntityKey)
		if err != nil {
			return nil, nil, err
		}
		return []domain.Entity{root}, map[string][]domain.SelectionReason{
			root.Key: {{Kind: domain.ReasonDirectEntity}},
		}, nil
	}
	match, err := ftsQuery(query.Anchor.Query)
	if err != nil {
		return nil, nil, err
	}
	candidates, found, err := searchCandidates(ctx, conn, key.WorktreeID, match, query.Anchor.Query)
	if err != nil {
		return nil, nil, err
	}
	if !found && containsNonASCII(query.Anchor.Query) {
		candidates, err = searchCandidatesByNoteBody(ctx, conn, key.WorktreeID, query.Anchor.Query, candidates)
		if err != nil {
			return nil, nil, err
		}
	}
	active := make(map[string][]domain.Note)
	for _, note := range notes {
		if note.BindingState == domain.NoteBindingActive {
			active[note.EntityKey] = append(active[note.EntityKey], note)
		}
	}
	byKey := make(map[string]domain.Entity, len(entities))
	for _, entity := range entities {
		byKey[entity.Key] = entity
	}
	rootsByKey := make(map[string]domain.Entity, len(candidates))
	reasons := make(map[string][]domain.SelectionReason, len(candidates))
	for _, candidate := range candidates {
		entity, exists := byKey[candidate.EntityKey]
		if !exists {
			continue
		}
		hit := evidenceSearchHit(candidate, strings.Fields(query.Anchor.Query), active[entity.Key])
		rootsByKey[entity.Key] = entity
		for _, field := range hit.MatchedFields {
			kind := domain.ReasonTextEntityField
			if field == domain.SearchMatchNoteBody {
				kind = domain.ReasonTextNoteBody
			}
			reasons[entity.Key] = append(reasons[entity.Key], domain.SelectionReason{Kind: kind, MatchedField: field, MatchedTerms: append([]string(nil), hit.MatchedTerms...)})
		}
	}
	for _, note := range notes {
		if note.BindingState != domain.NoteBindingActive || !noteHasTopics(note, query.Topics) {
			continue
		}
		entity, found := byKey[note.EntityKey]
		if !found {
			continue
		}
		rootsByKey[entity.Key] = entity
		reasons[entity.Key] = appendSelectionReason(reasons[entity.Key], domain.SelectionReason{Kind: domain.ReasonTopicExact, MatchedTerms: append([]string(nil), query.Topics...)})
	}
	roots := make([]domain.Entity, 0, len(rootsByKey))
	for _, root := range rootsByKey {
		roots = append(roots, root)
	}
	sort.Slice(roots, func(left, right int) bool { return roots[left].Key < roots[right].Key })
	return roots, reasons, nil
}

func noteHasTopics(note domain.Note, topics []string) bool {
	if len(topics) == 0 {
		return false
	}
	for _, topic := range topics {
		found := false
		for _, candidate := range note.Topics {
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

func appendSelectionReason(reasons []domain.SelectionReason, reason domain.SelectionReason) []domain.SelectionReason {
	for _, existing := range reasons {
		if existing.Kind == reason.Kind {
			return reasons
		}
	}
	return append(reasons, reason)
}

func candidatesForRoots(roots []domain.Entity, reasons map[string][]domain.SelectionReason, notes []domain.Note) []retrieval.Candidate {
	notesByEntity := make(map[string][]domain.Note)
	for _, note := range notes {
		notesByEntity[note.EntityKey] = append(notesByEntity[note.EntityKey], note)
	}
	var candidates []retrieval.Candidate
	for _, root := range roots {
		for _, note := range notesByEntity[root.Key] {
			candidates = appendNotes(candidates, root, []domain.Note{note}, reasons[root.Key])
		}
	}
	return candidates
}

func (s *Store) historicalCandidates(parentID string, key domain.WorkingSetKey, query domain.ContextQuery, roots []domain.Entity, reasons map[string][]domain.SelectionReason, changes []domain.EntityChange, current []domain.Note) ([]retrieval.Candidate, error) {
	graph, err := s.loadObjectGraph(parentID, key.RepositoryID, key.RefName)
	if err != nil {
		return nil, err
	}
	currentObservations := make(map[string]struct{}, len(current))
	for _, note := range current {
		currentObservations[noteObservationKey(note)] = struct{}{}
	}
	seen := make(map[string]struct{})
	var candidates []retrieval.Candidate
	for index := len(graph) - 1; index >= 0; index-- {
		item := graph[index]
		entities := make(map[string]domain.Entity, len(item.Object.Entities))
		for _, entity := range item.Object.Entities {
			entities[entity.Key] = entity
		}
		for _, note := range item.Object.Notes {
			observation := noteObservationKey(note)
			if _, current := currentObservations[observation]; current {
				continue
			}
			if _, duplicate := seen[observation]; duplicate {
				continue
			}
			if query.Anchor.Kind == domain.AnchorText {
				entity, found := entities[note.EntityKey]
				if !found {
					continue
				}
				reasons := historicalTextReasons(query, entity, note)
				if len(reasons) == 0 {
					continue
				}
				reasons = append(reasons, domain.SelectionReason{Kind: domain.ReasonHistoricalRevision})
				candidates = append(candidates, retrieval.Candidate{
					Root: entity, Bound: entity, Note: note, Reasons: reasons,
					Historical: true, ContextCommit: item.ID,
				})
				seen[observation] = struct{}{}
				continue
			}
			for _, root := range roots {
				if !containsStringValue(rootHistoryKeys(root, changes), note.EntityKey) {
					continue
				}
				bound := root
				if historicalEntity, found := entities[note.EntityKey]; found {
					bound = historicalEntity
				}
				candidateReasons := append([]domain.SelectionReason(nil), reasons[root.Key]...)
				candidateReasons = append(candidateReasons, domain.SelectionReason{Kind: domain.ReasonHistoricalRevision})
				candidates = append(candidates, retrieval.Candidate{
					Root: root, Bound: bound, Note: note, Reasons: candidateReasons,
					Historical: true, ContextCommit: item.ID,
				})
				seen[observation] = struct{}{}
				break
			}
		}
	}
	return candidates, nil
}

func rootHistoryKeys(root domain.Entity, changes []domain.EntityChange) []string {
	keys := []string{root.Key}
	for _, change := range changes {
		if change.Target.Key == root.Key && change.Base.Key != "" && change.Base.Key != change.Target.Key {
			keys = append(keys, change.Base.Key)
		}
	}
	return keys
}

func historicalTextReasons(query domain.ContextQuery, entity domain.Entity, note domain.Note) []domain.SelectionReason {
	terms := strings.Fields(query.Anchor.Query)
	var reasons []domain.SelectionReason
	if containsAllTerms(note.Body, terms) {
		reasons = append(reasons, domain.SelectionReason{Kind: domain.ReasonTextNoteBody, MatchedField: domain.SearchMatchNoteBody, MatchedTerms: append([]string(nil), terms...)})
	}
	for _, field := range []struct {
		kind  domain.SearchMatchField
		value string
	}{
		{kind: domain.SearchMatchEntityKey, value: entity.Key},
		{kind: domain.SearchMatchName, value: entity.Name},
		{kind: domain.SearchMatchSignature, value: entity.Signature},
		{kind: domain.SearchMatchPath, value: entity.Path},
	} {
		if containsAllTerms(field.value, terms) {
			reasons = append(reasons, domain.SelectionReason{Kind: domain.ReasonTextEntityField, MatchedField: field.kind, MatchedTerms: append([]string(nil), terms...)})
		}
	}
	if noteHasTopics(note, query.Topics) {
		reasons = append(reasons, domain.SelectionReason{Kind: domain.ReasonTopicExact, MatchedTerms: append([]string(nil), query.Topics...)})
	}
	return reasons
}

func containsAllTerms(value string, terms []string) bool {
	value = strings.ToLower(value)
	for _, term := range terms {
		if !strings.Contains(value, strings.ToLower(term)) {
			return false
		}
	}
	return len(terms) != 0
}

func noteObservationKey(note domain.Note) string {
	return strings.Join([]string{note.ID, note.RevisionID, note.EntityKey, string(note.BindingState), note.BindingSourceSHA}, "\x00")
}

func containsStringValue(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func appendNotes(candidates []retrieval.Candidate, root domain.Entity, notes []domain.Note, reasons []domain.SelectionReason) []retrieval.Candidate {
	for _, note := range notes {
		candidates = append(candidates, retrieval.Candidate{
			Root: root, Bound: root, Note: note,
			Reasons: append([]domain.SelectionReason(nil), reasons...),
		})
	}
	return candidates
}
