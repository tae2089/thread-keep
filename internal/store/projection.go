package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

// ApplyIndexUpdate atomically records coverage and replaces only successful language projections.
// A failed or missing pack never deletes its previous entities; current queries filter them out
// through the coverage row instead.
func (s *Store) ApplyIndexUpdate(ctx context.Context, key domain.WorkingSetKey, projections []domain.LanguageProjection) error {
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		return s.applyIndexUpdate(ctx, conn, key, projections, true)
	})
}

func (s *Store) applyIndexUpdate(ctx context.Context, conn *sql.Conn, key domain.WorkingSetKey, projections []domain.LanguageProjection, reconcileBindings bool) error {
	if err := s.ensureWorkingSet(ctx, conn, key); err != nil {
		return err
	}
	detected := make(map[string]struct{}, len(projections))
	for _, projection := range projections {
		detected[projection.Coverage.Language] = struct{}{}
	}
	if err := clearUndetectedCoverage(ctx, conn, key.WorktreeID, detected); err != nil {
		return err
	}
	for _, projection := range projections {
		coverage := projection.Coverage
		if err := validateProjection(key, coverage, projection.Entities); err != nil {
			return err
		}
		if coverage.State == domain.CoverageIndexed {
			if _, err := conn.ExecContext(ctx, "DELETE FROM entities WHERE worktree_id = ? AND language = ?", key.WorktreeID, coverage.Language); err != nil {
				return localError("clear language entities", err)
			}
			for _, entity := range projection.Entities {
				if _, err := conn.ExecContext(ctx, `INSERT INTO entities (worktree_id, language, entity_key, kind, name, signature, path, start_line, end_line, source_sha, structural_hash)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, key.WorktreeID, coverage.Language, entity.Key, entity.Kind, entity.Name, entity.Signature, entity.Path, entity.StartLine, entity.EndLine, entity.SourceSHA, entity.StructuralHash); err != nil {
					return localError("insert entity", err)
				}
			}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO language_coverage (worktree_id, language, state, indexer_id, indexer_version, source_sha, detail)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(worktree_id, language) DO UPDATE SET state = excluded.state, indexer_id = excluded.indexer_id, indexer_version = excluded.indexer_version, source_sha = excluded.source_sha, detail = excluded.detail`,
			key.WorktreeID, coverage.Language, coverage.State, coverage.IndexerID, coverage.IndexerVersion, coverage.SourceSHA, coverage.Detail); err != nil {
			return localError("upsert language coverage", err)
		}
	}
	if reconcileBindings {
		if err := s.reconcileActiveBindings(ctx, conn, key); err != nil {
			return err
		}
	}
	return s.rebuildSearch(ctx, conn, key)
}

func (s *Store) reconcileActiveBindings(ctx context.Context, conn *sql.Conn, key domain.WorkingSetKey) error {
	parentID, _, err := s.ref(ctx, conn, key.RefName)
	if err != nil {
		return err
	}
	if parentID == "" {
		return nil
	}
	graph, err := s.loadObjectGraph(parentID, key.RepositoryID, key.RefName)
	if err != nil {
		return err
	}
	parent := graph[len(graph)-1].Object
	parentEntities := make(map[string]domain.Entity, len(parent.Entities))
	for _, entity := range parent.Entities {
		parentEntities[entity.Key] = entity
	}
	currentEntities, err := listFreshEntities(ctx, conn, key.WorktreeID, key.SourceSHA)
	if err != nil {
		return err
	}
	reconciler := domain.NewBindingReconciler(currentEntities, key.SourceSHA)
	notes, err := listCommittedNotes(ctx, conn, parentID)
	if err != nil {
		return err
	}
	pending, err := listPendingNotes(ctx, conn, key.WorktreeID)
	if err != nil {
		return err
	}
	pendingIDs := make(map[string]struct{}, len(pending))
	for _, note := range pending {
		pendingIDs[note.ID] = struct{}{}
	}
	for _, note := range notes {
		if note.BindingState != domain.NoteBindingActive {
			continue
		}
		if _, found := pendingIDs[note.ID]; found {
			continue
		}
		updated, changed := reconciler.Reconcile(note, parentEntities[note.EntityKey])
		if !changed {
			continue
		}
		if err := upsertPendingNote(ctx, conn, key.WorktreeID, updated); err != nil {
			return err
		}
	}
	return nil
}

func clearUndetectedCoverage(ctx context.Context, conn *sql.Conn, worktreeID string, detected map[string]struct{}) error {
	rows, err := conn.QueryContext(ctx, "SELECT language FROM language_coverage WHERE worktree_id = ?", worktreeID)
	if err != nil {
		return localError("read existing language coverage", err)
	}
	var remove []string
	for rows.Next() {
		var language string
		if err := rows.Scan(&language); err != nil {
			_ = rows.Close()
			return localError("scan existing language coverage", err)
		}
		if _, found := detected[language]; !found {
			remove = append(remove, language)
		}
	}
	if err := rows.Close(); err != nil {
		return localError("close existing language coverage", err)
	}
	for _, language := range remove {
		if _, err := conn.ExecContext(ctx, "DELETE FROM language_coverage WHERE worktree_id = ? AND language = ?", worktreeID, language); err != nil {
			return localError("clear undetected language coverage", err)
		}
	}
	return nil
}

func (s *Store) rebuildSearch(ctx context.Context, conn *sql.Conn, key domain.WorkingSetKey) error {
	if _, err := conn.ExecContext(ctx, "DELETE FROM search_index WHERE worktree_id = ?", key.WorktreeID); err != nil {
		return localError("clear search index", err)
	}
	entities, err := listFreshEntities(ctx, conn, key.WorktreeID, key.SourceSHA)
	if err != nil {
		return err
	}
	parent, _, err := s.ref(ctx, conn, key.RefName)
	if err != nil {
		return err
	}
	var committed []domain.Note
	if parent != "" {
		committed, err = listCommittedNotes(ctx, conn, parent)
		if err != nil {
			return err
		}
	}
	pending, err := listPendingNotes(ctx, conn, key.WorktreeID)
	if err != nil {
		return err
	}
	activeByEntity := map[string][]string{}
	pendingByEntity := map[string]bool{}
	for _, note := range effectiveNotes(committed, pending) {
		if note.BindingState != domain.NoteBindingActive {
			continue
		}
		activeByEntity[note.EntityKey] = append(activeByEntity[note.EntityKey], note.Body)
		if note.Pending {
			pendingByEntity[note.EntityKey] = true
		}
	}
	for _, entity := range entities {
		bodies := append([]string{}, activeByEntity[entity.Key]...)
		state := "committed"
		if pendingByEntity[entity.Key] {
			state = "pending"
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO search_index (worktree_id, entity_key, name, signature, path, note_body, state)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, key.WorktreeID, entity.Key, entity.Name, entity.Signature, entity.Path, strings.Join(bodies, "\n"), state); err != nil {
			return localError("insert search document", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO context_commit_parents (context_commit_id, parent_index, parent_id)
		SELECT commit_id, 0, parent_id FROM context_commits WHERE parent_id != ''`); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("backfill context commit parents: %w", err))
	}
	return nil
}
