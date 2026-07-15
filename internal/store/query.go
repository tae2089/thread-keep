package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/tae2089/thread-keep/internal/domain"
)

const maxSearchCandidates = 100

const maxRelatedLimit = 100

type searchCandidate struct {
	EntityKey string
	Name      string
	Signature string
	Path      string
	NoteBody  string
	Pending   bool
	Snippet   string
	rank      float64
}

type rankedSearchHit struct {
	domain.SearchHit
	rank     float64
	priority int
}

func (s *Store) Status(ctx context.Context, key domain.WorkingSetKey) (domain.Status, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return domain.Status{}, localError("open SQLite connection", err)
	}
	defer conn.Close()
	status := domain.Status{RepositoryID: key.RepositoryID, WorktreeID: key.WorktreeID, RefName: key.RefName, SourceSHA: key.SourceSHA}
	_ = conn.QueryRowContext(ctx, "SELECT source_sha FROM working_sets WHERE worktree_id = ?", key.WorktreeID).Scan(&status.SourceSHA)
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM entities AS e
		JOIN language_coverage AS c ON c.worktree_id = e.worktree_id AND c.language = e.language
		WHERE e.worktree_id = ? AND c.state = ? AND c.source_sha = ?`, key.WorktreeID, domain.CoverageIndexed, status.SourceSHA).Scan(&status.EntityCount); err != nil {
		return domain.Status{}, localError("count entities", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_notes WHERE worktree_id = ?", key.WorktreeID).Scan(&status.PendingNotes); err != nil {
		return domain.Status{}, localError("count pending notes", err)
	}
	_ = conn.QueryRowContext(ctx, "SELECT commit_id FROM context_refs WHERE ref_name = ?", key.RefName).Scan(&status.ContextCommitID)
	coverage, err := listCoverage(ctx, conn, key.WorktreeID)
	if err != nil {
		return domain.Status{}, err
	}
	status.Coverage = coverage
	status.CoverageComplete = coverageComplete(coverage, status.SourceSHA)
	return status, nil
}

func (s *Store) CoverageComplete(ctx context.Context, key domain.WorkingSetKey) (bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, localError("open SQLite connection", err)
	}
	defer conn.Close()
	if err := s.requireWorkingSet(ctx, conn, key); err != nil {
		return false, err
	}
	coverage, err := listCoverage(ctx, conn, key.WorktreeID)
	if err != nil {
		return false, err
	}
	return coverageComplete(coverage, key.SourceSHA), nil
}

func (s *Store) Search(ctx context.Context, key domain.WorkingSetKey, query string) ([]domain.SearchHit, error) {
	query = strings.TrimSpace(query)
	terms := strings.Fields(query)
	match, err := ftsQuery(query)
	if err != nil {
		return nil, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, localError("open SQLite connection", err)
	}
	defer conn.Close()
	if err := s.requireWorkingSet(ctx, conn, key); err != nil {
		return nil, err
	}
	candidates, ftsFound, err := searchCandidates(ctx, conn, key.WorktreeID, match, query)
	if err != nil {
		return nil, err
	}
	if !ftsFound && containsNonASCII(query) {
		candidates, err = searchCandidatesByNoteBody(ctx, conn, key.WorktreeID, query, candidates)
		if err != nil {
			return nil, err
		}
	}
	parent, _, err := s.ref(ctx, conn, key.RefName)
	if err != nil {
		return nil, err
	}
	var committed []domain.Note
	if parent != "" {
		committed, err = listCommittedNotes(ctx, conn, parent)
		if err != nil {
			return nil, err
		}
	}
	pending, err := listPendingNotes(ctx, conn, key.WorktreeID)
	if err != nil {
		return nil, err
	}
	activeNotes := make(map[string][]domain.Note)
	for _, note := range effectiveNotes(committed, pending) {
		if note.BindingState == domain.NoteBindingActive {
			activeNotes[note.EntityKey] = append(activeNotes[note.EntityKey], note)
		}
	}
	hits := make([]rankedSearchHit, 0, len(candidates))
	for _, candidate := range candidates {
		hit := evidenceSearchHit(candidate, terms, activeNotes[candidate.EntityKey])
		hits = append(hits, rankedSearchHit{SearchHit: hit, rank: candidate.rank, priority: searchPriority(hit, query)})
	}
	sort.Slice(hits, func(left, right int) bool {
		if hits[left].priority != hits[right].priority {
			return hits[left].priority < hits[right].priority
		}
		if hits[left].rank != hits[right].rank {
			return hits[left].rank < hits[right].rank
		}
		return hits[left].EntityKey < hits[right].EntityKey
	})
	result := make([]domain.SearchHit, 0, len(hits))
	for _, hit := range hits {
		result = append(result, hit.SearchHit)
	}
	return result, nil
}

func searchCandidates(ctx context.Context, conn *sql.Conn, worktreeID, match, query string) (map[string]searchCandidate, bool, error) {
	candidates := make(map[string]searchCandidate)
	rows, err := conn.QueryContext(ctx, `SELECT entity_key, name, signature, path, note_body, state, snippet(search_index, 5, '[', ']', '…', 12), bm25(search_index)
		FROM search_index WHERE worktree_id = ? AND search_index MATCH ? ORDER BY rank LIMIT ?`, worktreeID, match, maxSearchCandidates)
	if err != nil {
		return nil, false, localError("search context", err)
	}
	found, err := scanSearchCandidates(rows, candidates)
	if err != nil {
		return nil, false, err
	}
	row := conn.QueryRowContext(ctx, `SELECT entity_key, name, signature, path, note_body, state, '', 0.0
		FROM search_index WHERE worktree_id = ? AND entity_key = ?`, worktreeID, query)
	var exact searchCandidate
	var state string
	if err := row.Scan(&exact.EntityKey, &exact.Name, &exact.Signature, &exact.Path, &exact.NoteBody, &state, &exact.Snippet, &exact.rank); err == nil {
		exact.Pending = state == "pending"
		candidates[exact.EntityKey] = exact
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, localError("read exact search key", err)
	}
	return candidates, found, nil
}

func searchCandidatesByNoteBody(ctx context.Context, conn *sql.Conn, worktreeID, query string, candidates map[string]searchCandidate) (map[string]searchCandidate, error) {
	rows, err := conn.QueryContext(ctx, `SELECT entity_key, name, signature, path, note_body, state, snippet(search_index, 5, '[', ']', '…', 12), 0.0
		FROM search_index WHERE worktree_id = ? AND note_body LIKE ? ORDER BY entity_key LIMIT ?`, worktreeID, "%"+query+"%", maxSearchCandidates)
	if err != nil {
		return nil, localError("fallback search context", err)
	}
	_, err = scanSearchCandidates(rows, candidates)
	return candidates, err
}

func scanSearchCandidates(rows *sql.Rows, candidates map[string]searchCandidate) (bool, error) {
	defer rows.Close()
	found := false
	for rows.Next() {
		var candidate searchCandidate
		var state string
		if err := rows.Scan(&candidate.EntityKey, &candidate.Name, &candidate.Signature, &candidate.Path, &candidate.NoteBody, &state, &candidate.Snippet, &candidate.rank); err != nil {
			return false, localError("scan search result", err)
		}
		candidate.Pending = state == "pending"
		candidates[candidate.EntityKey] = candidate
		found = true
	}
	if err := rows.Err(); err != nil {
		return false, localError("iterate search results", err)
	}
	return found, nil
}

func evidenceSearchHit(candidate searchCandidate, terms []string, notes []domain.Note) domain.SearchHit {
	fields := make([]domain.SearchMatchField, 0, 5)
	matchedTerms := make([]string, 0, len(terms))
	noteIDs := make([]string, 0, len(notes))
	for _, term := range terms {
		matched := false
		if containsSearchTerm(candidate.EntityKey, term) {
			fields = appendMatchField(fields, domain.SearchMatchEntityKey)
			matched = true
		}
		if containsSearchTerm(candidate.Name, term) {
			fields = appendMatchField(fields, domain.SearchMatchName)
			matched = true
		}
		if containsSearchTerm(candidate.Signature, term) {
			fields = appendMatchField(fields, domain.SearchMatchSignature)
			matched = true
		}
		if containsSearchTerm(candidate.Path, term) {
			fields = appendMatchField(fields, domain.SearchMatchPath)
			matched = true
		}
		for _, note := range notes {
			if containsSearchTerm(note.Body, term) {
				fields = appendMatchField(fields, domain.SearchMatchNoteBody)
				noteIDs = appendUniqueString(noteIDs, note.ID)
				matched = true
			}
		}
		if matched {
			matchedTerms = append(matchedTerms, term)
		}
	}
	hit := domain.SearchHit{EntityKey: candidate.EntityKey, Name: candidate.Name, Path: candidate.Path, Pending: candidate.Pending, Snippet: candidate.Snippet, MatchedFields: fields, MatchedTerms: matchedTerms, NoteIDs: noteIDs, Fresh: true}
	if len(noteIDs) > 0 {
		hit.BindingState = domain.NoteBindingActive
	}
	return hit
}

func containsSearchTerm(value, term string) bool {
	value = strings.ToLower(value)
	term = strings.ToLower(term)
	if strings.Contains(value, term) {
		return true
	}
	terms := strings.FieldsFunc(term, func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsNumber(character) && character != '_'
	})
	if len(terms) == 0 {
		return false
	}
	for _, normalizedTerm := range terms {
		if !strings.Contains(value, normalizedTerm) {
			return false
		}
	}
	return true
}

func appendMatchField(fields []domain.SearchMatchField, field domain.SearchMatchField) []domain.SearchMatchField {
	for _, existing := range fields {
		if existing == field {
			return fields
		}
	}
	return append(fields, field)
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func searchPriority(hit domain.SearchHit, query string) int {
	if strings.EqualFold(hit.EntityKey, strings.TrimSpace(query)) {
		return 0
	}
	if len(hit.MatchedTerms) == 1 && strings.EqualFold(hit.Name, hit.MatchedTerms[0]) {
		return 1
	}
	for _, field := range hit.MatchedFields {
		if field == domain.SearchMatchNoteBody {
			return 2
		}
	}
	for _, field := range hit.MatchedFields {
		if field == domain.SearchMatchSignature || field == domain.SearchMatchPath {
			return 3
		}
	}
	return 4
}

func (s *Store) Related(ctx context.Context, key domain.WorkingSetKey, entityKey string, limit int) ([]domain.RelatedEntity, error) {
	if strings.TrimSpace(entityKey) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("entity key must not be empty"))
	}
	if limit < 1 || limit > maxRelatedLimit {
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("related context limit must be between 1 and %d", maxRelatedLimit))
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, localError("open SQLite connection", err)
	}
	defer conn.Close()
	if err := s.requireWorkingSet(ctx, conn, key); err != nil {
		return nil, err
	}
	root, err := entityByKeyFresh(ctx, conn, key.WorktreeID, key.SourceSHA, entityKey)
	if err != nil {
		return nil, err
	}
	entities, err := listFreshEntities(ctx, conn, key.WorktreeID, key.SourceSHA)
	if err != nil {
		return nil, err
	}
	result := relatedFromEntities(root, entities)
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func relatedFromEntities(root domain.Entity, entities []domain.Entity) []domain.RelatedEntity {
	byKey := make(map[string]domain.Entity, len(entities))
	for _, entity := range entities {
		byKey[entity.Key] = entity
	}
	related := make(map[string]domain.RelatedEntity)
	if root.Kind == domain.EntityMethod {
		if owner, found := methodOwner(root, byKey); found {
			related[owner.Key] = relatedEntity(owner, "method_owner")
		}
	}
	if ownerKind(root.Kind) {
		for _, entity := range entities {
			if entity.Kind == domain.EntityMethod && methodOwnerKey(entity.Key) == root.Key {
				related[entity.Key] = relatedEntity(entity, "method_owner")
			}
		}
	}
	for _, entity := range entities {
		if entity.Key == root.Key || entity.Path != root.Path {
			continue
		}
		if _, found := related[entity.Key]; !found {
			related[entity.Key] = relatedEntity(entity, "same_file")
		}
	}
	result := make([]domain.RelatedEntity, 0, len(related))
	for _, entity := range related {
		result = append(result, entity)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].EdgeKind != result[right].EdgeKind {
			return result[left].EdgeKind < result[right].EdgeKind
		}
		return result[left].EntityKey < result[right].EntityKey
	})
	return result
}

func methodOwnerKey(entityKey string) string {
	separator := strings.LastIndex(entityKey, ".")
	if separator <= 0 {
		return ""
	}
	return entityKey[:separator]
}

func methodOwner(method domain.Entity, byKey map[string]domain.Entity) (domain.Entity, bool) {
	if owner, found := byKey[methodOwnerKey(method.Key)]; found && ownerKind(owner.Kind) {
		return owner, true
	}
	const methodMarker = "#method:"
	marker := strings.Index(method.Key, methodMarker)
	if marker < 0 {
		return domain.Entity{}, false
	}
	qualifiedName := method.Key[marker+len(methodMarker):]
	separator := strings.LastIndex(qualifiedName, ".")
	if separator <= 0 {
		return domain.Entity{}, false
	}
	prefix := method.Key[:marker]
	ownerName := qualifiedName[:separator]
	for _, kind := range []domain.EntityKind{domain.EntityClass, domain.EntityInterface, domain.EntityType} {
		key := prefix + "#" + string(kind) + ":" + ownerName
		if owner, found := byKey[key]; found && ownerKind(owner.Kind) {
			return owner, true
		}
	}
	return domain.Entity{}, false
}

func ownerKind(kind domain.EntityKind) bool {
	return kind == domain.EntityType || kind == domain.EntityClass || kind == domain.EntityInterface
}

func relatedEntity(entity domain.Entity, edgeKind string) domain.RelatedEntity {
	return domain.RelatedEntity{EntityKey: entity.Key, Name: entity.Name, Path: entity.Path, EdgeKind: edgeKind, Fresh: true}
}

func (s *Store) Context(ctx context.Context, key domain.WorkingSetKey, entityKey string) (domain.Entity, []domain.Note, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return domain.Entity{}, nil, localError("open SQLite connection", err)
	}
	defer conn.Close()
	if err := s.requireWorkingSet(ctx, conn, key); err != nil {
		return domain.Entity{}, nil, err
	}
	entity, err := entityByKeyFresh(ctx, conn, key.WorktreeID, key.SourceSHA, entityKey)
	if err != nil {
		return domain.Entity{}, nil, err
	}
	parent, _, err := s.ref(ctx, conn, key.RefName)
	if err != nil {
		return domain.Entity{}, nil, err
	}
	var committed []domain.Note
	if parent != "" {
		committed, err = listCommittedNotes(ctx, conn, parent)
		if err != nil {
			return domain.Entity{}, nil, err
		}
	}
	pending, err := listPendingNotes(ctx, conn, key.WorktreeID)
	if err != nil {
		return domain.Entity{}, nil, err
	}
	var notes []domain.Note
	for _, note := range effectiveNotes(committed, pending) {
		if note.EntityKey == entityKey && note.BindingState == domain.NoteBindingActive {
			notes = append(notes, note)
		}
	}
	return entity, notes, nil
}

func (s *Store) PendingNotes(ctx context.Context, key domain.WorkingSetKey) ([]domain.Note, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, localError("open SQLite connection", err)
	}
	defer conn.Close()
	return listPendingNotes(ctx, conn, key.WorktreeID)
}

func (s *Store) Log(ctx context.Context, key domain.WorkingSetKey, limit int) ([]domain.ContextCommit, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT commit_id, parent_id, ref_name, source_sha, message, author, created_at
		FROM context_commits WHERE ref_name = ? ORDER BY created_at DESC LIMIT ?`, key.RefName, limit)
	if err != nil {
		return nil, localError("read context log", err)
	}
	defer rows.Close()
	var commits []domain.ContextCommit
	for rows.Next() {
		var commit domain.ContextCommit
		var createdAt int64
		if err := rows.Scan(&commit.ID, &commit.ParentID, &commit.RefName, &commit.SourceSHA, &commit.Message, &commit.Author, &createdAt); err != nil {
			return nil, localError("scan context log", err)
		}
		commit.CreatedAt = time.Unix(0, createdAt).UTC()
		commits = append(commits, commit)
	}
	return commits, rows.Err()
}

func ftsQuery(query string) (string, error) {
	tokens := strings.Fields(query)
	if len(tokens) == 0 {
		return "", domain.NewError(domain.CodeValidation, errors.New("search query must not be empty"))
	}
	for index, token := range tokens {
		tokens[index] = `"` + strings.ReplaceAll(token, `"`, `""`) + `"`
	}
	return strings.Join(tokens, " AND "), nil
}

func containsNonASCII(value string) bool {
	for _, character := range value {
		if character > 127 {
			return true
		}
	}
	return false
}
