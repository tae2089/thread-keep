package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

func (s *Store) Initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, "CREATE VIRTUAL TABLE IF NOT EXISTS fts5_capability_check USING fts5(value)"); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("SQLite FTS5 is unavailable; build with sqlite_fts5: %w", err))
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE fts5_capability_check"); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("clean FTS5 capability check: %w", err))
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"DROP TABLE IF EXISTS entity_relations",
		"DROP TABLE IF EXISTS relation_coverage",
		`CREATE TABLE IF NOT EXISTS working_sets (
			worktree_id TEXT PRIMARY KEY,
			repository_id TEXT NOT NULL,
			ref_name TEXT NOT NULL,
			base_context_commit_id TEXT NOT NULL,
			source_sha TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS entities (
			worktree_id TEXT NOT NULL,
			language TEXT NOT NULL DEFAULT 'go',
			entity_key TEXT NOT NULL,
			kind TEXT NOT NULL,
			name TEXT NOT NULL,
			signature TEXT NOT NULL,
			path TEXT NOT NULL,
			start_line INTEGER NOT NULL,
			end_line INTEGER NOT NULL,
			source_sha TEXT NOT NULL,
			structural_hash TEXT NOT NULL,
			PRIMARY KEY (worktree_id, entity_key)
		)`,
		`CREATE TABLE IF NOT EXISTS language_coverage (
			worktree_id TEXT NOT NULL,
			language TEXT NOT NULL,
			state TEXT NOT NULL,
			indexer_id TEXT NOT NULL,
			indexer_version TEXT NOT NULL,
			source_sha TEXT NOT NULL,
			detail TEXT NOT NULL,
			PRIMARY KEY (worktree_id, language)
		)`,
		`CREATE TABLE IF NOT EXISTS pending_notes (
			worktree_id TEXT NOT NULL,
			note_id TEXT NOT NULL,
			revision_id TEXT NOT NULL DEFAULT '',
			supersedes_revision_id TEXT NOT NULL DEFAULT '',
			entity_key TEXT NOT NULL,
			kind TEXT NOT NULL,
			body TEXT NOT NULL,
			author TEXT NOT NULL,
			origin TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			binding_state TEXT NOT NULL DEFAULT '',
				binding_source_sha TEXT NOT NULL DEFAULT '',
				review_reason TEXT NOT NULL DEFAULT '',
				topics_json TEXT NOT NULL DEFAULT '[]',
				PRIMARY KEY (worktree_id, note_id)
		)`,
		`CREATE TABLE IF NOT EXISTS context_commits (
			commit_id TEXT PRIMARY KEY,
			parent_id TEXT NOT NULL,
			ref_name TEXT NOT NULL,
			source_sha TEXT NOT NULL,
			message TEXT NOT NULL,
			author TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS context_commit_parents (
			context_commit_id TEXT NOT NULL,
			parent_index INTEGER NOT NULL,
			parent_id TEXT NOT NULL,
			PRIMARY KEY (context_commit_id, parent_index)
		)`,
		`CREATE TABLE IF NOT EXISTS merge_sessions (
			session_id TEXT PRIMARY KEY,
			local_snapshot_id TEXT NOT NULL,
			remote_snapshot_id TEXT NOT NULL,
			base_snapshot_id TEXT NOT NULL,
			repository_id TEXT NOT NULL,
			ref_name TEXT NOT NULL,
			source_sha TEXT NOT NULL,
			provenance_json BLOB NOT NULL,
			message TEXT NOT NULL,
			author TEXT NOT NULL,
			planned_created_at INTEGER NOT NULL,
			state TEXT NOT NULL,
			automatic_records_json BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS merge_conflicts (
			session_id TEXT NOT NULL,
			conflict_id TEXT NOT NULL,
			conflict_json BLOB NOT NULL,
			PRIMARY KEY (session_id, conflict_id)
		)`,
		`CREATE TABLE IF NOT EXISTS landing_sessions (
			session_id TEXT PRIMARY KEY,
			landing_id TEXT NOT NULL UNIQUE,
			version INTEGER NOT NULL,
			state TEXT NOT NULL,
			payload BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS context_refs (
			ref_name TEXT PRIMARY KEY,
			commit_id TEXT NOT NULL,
			source_sha TEXT NOT NULL,
			version INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS remotes (
			remote_name TEXT PRIMARY KEY,
			path TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS remote_refs (
			remote_name TEXT NOT NULL,
			ref_name TEXT NOT NULL,
			commit_id TEXT NOT NULL,
			source_sha TEXT NOT NULL,
			version INTEGER NOT NULL,
			PRIMARY KEY (remote_name, ref_name)
		)`,
		`CREATE TABLE IF NOT EXISTS candidates (
			candidate_id TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			repository TEXT NOT NULL,
			number INTEGER NOT NULL,
			state TEXT NOT NULL,
			base_sha TEXT NOT NULL,
			head_sha TEXT NOT NULL,
			merge_sha TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			payload_hash TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS candidate_notes (
			candidate_id TEXT NOT NULL,
			note_id TEXT NOT NULL,
			entity_key TEXT NOT NULL,
			structural_hash TEXT NOT NULL,
			kind TEXT NOT NULL,
			body TEXT NOT NULL,
			author TEXT NOT NULL,
			origin TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			state TEXT NOT NULL,
			promoted_note_id TEXT NOT NULL,
			PRIMARY KEY (candidate_id, note_id)
		)`,
		`CREATE TABLE IF NOT EXISTS committed_notes (
			context_commit_id TEXT NOT NULL,
			note_id TEXT NOT NULL,
			revision_id TEXT NOT NULL DEFAULT '',
			supersedes_revision_id TEXT NOT NULL DEFAULT '',
			entity_key TEXT NOT NULL,
			kind TEXT NOT NULL,
			body TEXT NOT NULL,
			author TEXT NOT NULL,
			origin TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			binding_state TEXT NOT NULL DEFAULT '',
				binding_source_sha TEXT NOT NULL DEFAULT '',
				review_reason TEXT NOT NULL DEFAULT '',
				topics_json TEXT NOT NULL DEFAULT '[]',
				PRIMARY KEY (context_commit_id, note_id)
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS search_index USING fts5(
			worktree_id UNINDEXED,
			entity_key UNINDEXED,
			name,
			signature,
			path,
			note_body,
			state UNINDEXED
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("migrate SQLite schema: %w", err))
		}
	}
	if err := s.ensureColumn(ctx, "entities", "language", "TEXT NOT NULL DEFAULT 'go'"); err != nil {
		return err
	}
	for _, table := range []string{"pending_notes", "committed_notes"} {
		for _, column := range []struct{ name, definition string }{
			{name: "revision_id", definition: "TEXT NOT NULL DEFAULT ''"},
			{name: "supersedes_revision_id", definition: "TEXT NOT NULL DEFAULT ''"},
			{name: "binding_state", definition: "TEXT NOT NULL DEFAULT ''"},
			{name: "binding_source_sha", definition: "TEXT NOT NULL DEFAULT ''"},
			{name: "review_reason", definition: "TEXT NOT NULL DEFAULT ''"},
			{name: "topics_json", definition: "TEXT NOT NULL DEFAULT '[]'"},
		} {
			if err := s.ensureColumn(ctx, table, column.name, column.definition); err != nil {
				return err
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO language_coverage (worktree_id, language, state, indexer_id, indexer_version, source_sha, detail)
		SELECT w.worktree_id, 'go', 'indexed', 'builtin/go', '1', w.source_sha, ''
		FROM working_sets AS w
		WHERE EXISTS (SELECT 1 FROM entities AS e WHERE e.worktree_id = w.worktree_id AND e.language = 'go')
		ON CONFLICT(worktree_id, language) DO NOTHING`); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("migrate legacy Go coverage: %w", err))
	}
	return s.Initialize(ctx)
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("inspect %s columns: %w", table, err))
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("scan %s columns: %w", table, err))
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("read %s columns: %w", table, err))
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("add %s.%s: %w", table, column, err))
	}
	return nil
}

func (s *Store) ensureWorkingSet(ctx context.Context, conn *sql.Conn, key domain.WorkingSetKey) error {
	var existingRef, existingSHA string
	err := conn.QueryRowContext(ctx, "SELECT ref_name, source_sha FROM working_sets WHERE worktree_id = ?", key.WorktreeID).Scan(&existingRef, &existingSHA)
	if err == nil {
		if existingRef == key.RefName && existingSHA == key.SourceSHA {
			return nil
		}
		var pending int
		if countErr := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_notes WHERE worktree_id = ?", key.WorktreeID).Scan(&pending); countErr != nil {
			return localError("count pending notes", countErr)
		}
		if pending > 0 {
			return domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit or discard pending context changes before changing branch or source revision"))
		}
		parent, _, refErr := s.ref(ctx, conn, key.RefName)
		if refErr != nil {
			return refErr
		}
		_, updateErr := conn.ExecContext(ctx, `UPDATE working_sets SET repository_id = ?, ref_name = ?, base_context_commit_id = ?, source_sha = ?, updated_at = ? WHERE worktree_id = ?`, key.RepositoryID, key.RefName, parent, key.SourceSHA, time.Now().UTC().UnixNano(), key.WorktreeID)
		return localError("update working set", updateErr)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return localError("read working set", err)
	}
	parent, _, refErr := s.ref(ctx, conn, key.RefName)
	if refErr != nil {
		return refErr
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO working_sets (worktree_id, repository_id, ref_name, base_context_commit_id, source_sha, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, key.WorktreeID, key.RepositoryID, key.RefName, parent, key.SourceSHA, time.Now().UTC().UnixNano())
	return localError("create working set", err)
}

func (s *Store) requireWorkingSet(ctx context.Context, conn *sql.Conn, key domain.WorkingSetKey) error {
	var repositoryID, refName, sourceSHA string
	err := conn.QueryRowContext(ctx, "SELECT repository_id, ref_name, source_sha FROM working_sets WHERE worktree_id = ?", key.WorktreeID).Scan(&repositoryID, &refName, &sourceSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.NewError(domain.CodeStaleWorkingSet, errors.New("run update before changing context"))
	}
	if err != nil {
		return localError("read working set", err)
	}
	if repositoryID != key.RepositoryID || refName != key.RefName || sourceSHA != key.SourceSHA {
		return domain.NewError(domain.CodeStaleWorkingSet, errors.New("working set belongs to a different repository, worktree, branch or source revision"))
	}
	return nil
}

func (s *Store) ref(ctx context.Context, conn *sql.Conn, refName string) (string, int, error) {
	var commitID string
	var version int
	err := conn.QueryRowContext(ctx, "SELECT commit_id, version FROM context_refs WHERE ref_name = ?", refName).Scan(&commitID, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, localError("read context ref", err)
	}
	return commitID, version, nil
}

func listEntities(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, worktreeID string) ([]domain.Entity, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT language, entity_key, kind, name, signature, path, start_line, end_line, source_sha, structural_hash
		FROM entities WHERE worktree_id = ? ORDER BY entity_key`, worktreeID)
	if err != nil {
		return nil, localError("read entities", err)
	}
	defer rows.Close()
	var entities []domain.Entity
	for rows.Next() {
		var entity domain.Entity
		if err := rows.Scan(&entity.Language, &entity.Key, &entity.Kind, &entity.Name, &entity.Signature, &entity.Path, &entity.StartLine, &entity.EndLine, &entity.SourceSHA, &entity.StructuralHash); err != nil {
			return nil, localError("scan entity", err)
		}
		entities = append(entities, entity)
	}
	return entities, rows.Err()
}

func listFreshEntities(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, worktreeID, sourceSHA string) ([]domain.Entity, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT e.language, e.entity_key, e.kind, e.name, e.signature, e.path, e.start_line, e.end_line, e.source_sha, e.structural_hash
		FROM entities AS e
		JOIN language_coverage AS c ON c.worktree_id = e.worktree_id AND c.language = e.language
		WHERE e.worktree_id = ? AND c.state = ? AND c.source_sha = ?
		ORDER BY e.entity_key`, worktreeID, domain.CoverageIndexed, sourceSHA)
	if err != nil {
		return nil, localError("read fresh entities", err)
	}
	defer rows.Close()
	var entities []domain.Entity
	for rows.Next() {
		var entity domain.Entity
		if err := rows.Scan(&entity.Language, &entity.Key, &entity.Kind, &entity.Name, &entity.Signature, &entity.Path, &entity.StartLine, &entity.EndLine, &entity.SourceSHA, &entity.StructuralHash); err != nil {
			return nil, localError("scan fresh entity", err)
		}
		entities = append(entities, entity)
	}
	return entities, rows.Err()
}

func entityByKeyFresh(ctx context.Context, conn *sql.Conn, worktreeID, sourceSHA, entityKey string) (domain.Entity, error) {
	var entity domain.Entity
	err := conn.QueryRowContext(ctx, `SELECT e.language, e.entity_key, e.kind, e.name, e.signature, e.path, e.start_line, e.end_line, e.source_sha, e.structural_hash
		FROM entities AS e
		JOIN language_coverage AS c ON c.worktree_id = e.worktree_id AND c.language = e.language
		WHERE e.worktree_id = ? AND e.entity_key = ? AND c.state = ? AND c.source_sha = ?`, worktreeID, entityKey, domain.CoverageIndexed, sourceSHA).Scan(&entity.Language, &entity.Key, &entity.Kind, &entity.Name, &entity.Signature, &entity.Path, &entity.StartLine, &entity.EndLine, &entity.SourceSHA, &entity.StructuralHash)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Entity{}, domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("entity %q is not indexed", entityKey))
	}
	if err != nil {
		return domain.Entity{}, localError("read entity", err)
	}
	return entity, nil
}

func listCoverage(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, worktreeID string) ([]domain.Coverage, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT language, state, indexer_id, indexer_version, source_sha, detail
		FROM language_coverage WHERE worktree_id = ? ORDER BY language`, worktreeID)
	if err != nil {
		return nil, localError("read language coverage", err)
	}
	defer rows.Close()
	var coverage []domain.Coverage
	for rows.Next() {
		var item domain.Coverage
		if err := rows.Scan(&item.Language, &item.State, &item.IndexerID, &item.IndexerVersion, &item.SourceSHA, &item.Detail); err != nil {
			return nil, localError("scan language coverage", err)
		}
		coverage = append(coverage, item)
	}
	if err := rows.Err(); err != nil {
		return nil, localError("iterate language coverage", err)
	}
	return coverage, nil
}

func coverageComplete(coverage []domain.Coverage, sourceSHA string) bool {
	for _, item := range coverage {
		if item.State != domain.CoverageIndexed || item.SourceSHA != sourceSHA {
			return false
		}
	}
	return true
}

func listPendingNotes(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, worktreeID string) ([]domain.Note, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT p.note_id, p.revision_id, p.supersedes_revision_id, p.entity_key, p.kind, p.body, p.author, p.origin, p.created_at, p.binding_state, p.binding_source_sha, p.review_reason, p.topics_json, w.source_sha
		FROM pending_notes AS p JOIN working_sets AS w ON w.worktree_id = p.worktree_id
		WHERE p.worktree_id = ? ORDER BY p.created_at, p.note_id`, worktreeID)
	if err != nil {
		return nil, localError("read pending notes", err)
	}
	defer rows.Close()
	return scanNotes(rows, true)
}

func pendingNoteIDs(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, worktreeID string) ([]string, error) {
	rows, err := queryer.QueryContext(ctx, "SELECT note_id FROM pending_notes WHERE worktree_id = ? ORDER BY note_id", worktreeID)
	if err != nil {
		return nil, localError("read pending note IDs", err)
	}
	defer rows.Close()
	var identifiers []string
	for rows.Next() {
		var identifier string
		if err := rows.Scan(&identifier); err != nil {
			return nil, localError("scan pending note ID", err)
		}
		identifiers = append(identifiers, identifier)
	}
	if err := rows.Err(); err != nil {
		return nil, localError("iterate pending note IDs", err)
	}
	return identifiers, nil
}

func sameIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func listCommittedNotes(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, commitID string) ([]domain.Note, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT n.note_id, n.revision_id, n.supersedes_revision_id, n.entity_key, n.kind, n.body, n.author, n.origin, n.created_at, n.binding_state, n.binding_source_sha, n.review_reason, n.topics_json, c.source_sha
		FROM committed_notes AS n JOIN context_commits AS c ON c.commit_id = n.context_commit_id
		WHERE n.context_commit_id = ? ORDER BY n.created_at, n.note_id`, commitID)
	if err != nil {
		return nil, localError("read committed notes", err)
	}
	defer rows.Close()
	return scanNotes(rows, false)
}

func scanNotes(rows *sql.Rows, pending bool) ([]domain.Note, error) {
	var notes []domain.Note
	for rows.Next() {
		note, _, err := scanNote(rows, pending)
		if err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

func scanNote(scanner interface{ Scan(...any) error }, pending bool) (domain.Note, bool, error) {
	var note domain.Note
	var createdAt int64
	var topicsJSON string
	var sourceSHA string
	if err := scanner.Scan(&note.ID, &note.RevisionID, &note.SupersedesRevisionID, &note.EntityKey, &note.Kind, &note.Body, &note.Author, &note.Origin, &createdAt, &note.BindingState, &note.BindingSourceSHA, &note.ReviewReason, &topicsJSON, &sourceSHA); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Note{}, false, nil
		}
		return domain.Note{}, false, localError("scan note", err)
	}
	note.CreatedAt = time.Unix(0, createdAt).UTC()
	if err := json.Unmarshal([]byte(topicsJSON), &note.Topics); err != nil {
		return domain.Note{}, false, localError("decode note topics", err)
	}
	note = domain.NormalizeLegacyNote(note, sourceSHA)
	note.Pending = pending
	return note, true, nil
}

func (s *Store) withImmediate(ctx context.Context, operation func(*sql.Conn) error) (returned error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return localError("open SQLite connection", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return localError("begin SQLite immediate transaction", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := operation(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return localError("commit SQLite transaction", err)
	}
	committed = true
	return nil
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := io.ReadFull(rand.Reader, bytes[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", bytes[:]), nil
}

func localError(action string, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "database is locked") || strings.Contains(strings.ToLower(err.Error()), "database is busy") {
		return domain.NewError(domain.CodeBusy, fmt.Errorf("%s: %w", action, err))
	}
	return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("%s: %w", action, err))
}
