package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

type FastForwardInput struct {
	Key      domain.WorkingSetKey
	Expected domain.ContextRef
	Next     domain.ContextRef
}

type PrepareLandingRecoveryInput struct {
	Key      domain.WorkingSetKey
	Expected domain.ContextRef
	Next     domain.ContextRef
}

func (s *Store) ContextRef(ctx context.Context, key domain.WorkingSetKey) (domain.ContextRef, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return domain.ContextRef{}, localError("open SQLite connection", err)
	}
	defer conn.Close()
	if err := s.requireWorkingSet(ctx, conn, key); err != nil {
		return domain.ContextRef{}, err
	}
	return currentContextRef(ctx, conn, key.RefName)
}

func (s *Store) FastForward(ctx context.Context, input FastForwardInput) error {
	if input.Next.RefName != input.Key.RefName || input.Next.CommitID == "" || input.Next.SourceSHA != input.Key.SourceSHA {
		return domain.NewError(domain.CodeValidation, errors.New("fast-forward target does not match the current context ref and source"))
	}
	chain, err := s.loadObjectGraph(input.Next.CommitID, input.Key.RepositoryID, input.Key.RefName)
	if err != nil {
		return err
	}
	if chain[len(chain)-1].Object.SourceSHA != input.Key.SourceSHA {
		return domain.NewError(domain.CodeStaleWorkingSet, errors.New("remote context tip does not match the current Git source"))
	}
	if input.Expected.CommitID != "" {
		found := false
		for _, item := range chain {
			if item.ID == input.Expected.CommitID {
				found = true
				break
			}
		}
		if !found {
			return domain.NewError(domain.CodeRemoteConflict, errors.New("remote context does not fast-forward the local context ref"))
		}
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if err := s.requireWorkingSet(ctx, conn, input.Key); err != nil {
			return err
		}
		actual, err := currentContextRef(ctx, conn, input.Key.RefName)
		if err != nil {
			return err
		}
		if actual != input.Expected {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local context ref changed while pulling"))
		}
		var pendingCount int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_notes WHERE worktree_id = ?", input.Key.WorktreeID).Scan(&pendingCount); err != nil {
			return localError("count pending notes before pull", err)
		}
		if pendingCount != 0 {
			return domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit or discard pending context changes before pulling"))
		}
		if err := insertContextChain(ctx, conn, chain); err != nil {
			return err
		}
		if actual.CommitID == "" {
			if _, err := conn.ExecContext(ctx, "INSERT INTO context_refs (ref_name, commit_id, source_sha, version) VALUES (?, ?, ?, 1)", input.Key.RefName, input.Next.CommitID, input.Next.SourceSHA); err != nil {
				return localError("create pulled context ref", err)
			}
		} else {
			result, err := conn.ExecContext(ctx, "UPDATE context_refs SET commit_id = ?, source_sha = ?, version = version + 1 WHERE ref_name = ? AND commit_id = ? AND version = ?", input.Next.CommitID, input.Next.SourceSHA, input.Key.RefName, actual.CommitID, actual.Version)
			if err != nil {
				return localError("fast-forward local context ref", err)
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return localError("check local context ref compare-and-swap", err)
			}
			if changed != 1 {
				return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local context ref compare-and-swap failed while pulling"))
			}
		}
		result, err := conn.ExecContext(ctx, "UPDATE working_sets SET base_context_commit_id = ? WHERE worktree_id = ? AND ref_name = ? AND source_sha = ?", input.Next.CommitID, input.Key.WorktreeID, input.Key.RefName, input.Key.SourceSHA)
		if err != nil {
			return localError("advance pulled working set parent", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return localError("check pulled working set", err)
		}
		if changed != 1 {
			return domain.NewError(domain.CodeStaleWorkingSet, errors.New("working set changed while pulling"))
		}
		return s.rebuildSearch(ctx, conn, input.Key)
	})
}

func (s *Store) PrepareLandingRecovery(ctx context.Context, input PrepareLandingRecoveryInput) error {
	if input.Next.RefName != input.Key.RefName || input.Next.CommitID == "" || input.Next.SourceSHA == "" {
		return domain.NewError(domain.CodeValidation, errors.New("landing recovery target is incomplete or mismatched"))
	}
	chain, err := s.loadObjectGraph(input.Next.CommitID, input.Key.RepositoryID, input.Key.RefName)
	if err != nil {
		return err
	}
	if chain[len(chain)-1].Object.SourceSHA != input.Next.SourceSHA {
		return domain.NewError(domain.CodeValidation, errors.New("landing recovery ref source does not match its immutable object"))
	}
	if input.Expected.CommitID != "" {
		found := false
		for _, item := range chain {
			if item.ID == input.Expected.CommitID {
				found = true
				break
			}
		}
		if !found {
			return domain.NewError(domain.CodeRemoteConflict, errors.New("landing recovery context does not fast-forward the local context ref"))
		}
	}
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		if err := s.requireWorkingSet(ctx, conn, input.Key); err != nil {
			return err
		}
		actual, err := currentContextRef(ctx, conn, input.Key.RefName)
		if err != nil {
			return err
		}
		if actual != input.Expected {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local context ref changed while preparing landing recovery"))
		}
		var pendingCount int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pending_notes WHERE worktree_id = ?", input.Key.WorktreeID).Scan(&pendingCount); err != nil {
			return localError("count pending notes before landing recovery", err)
		}
		if pendingCount != 0 {
			return domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit or discard pending context changes before landing recovery"))
		}
		if err := insertContextChain(ctx, conn, chain); err != nil {
			return err
		}
		if actual.CommitID == "" {
			if _, err := conn.ExecContext(ctx, "INSERT INTO context_refs (ref_name, commit_id, source_sha, version) VALUES (?, ?, ?, 1)", input.Key.RefName, input.Next.CommitID, input.Next.SourceSHA); err != nil {
				return localError("create landing recovery context ref", err)
			}
		} else {
			result, err := conn.ExecContext(ctx, "UPDATE context_refs SET commit_id = ?, source_sha = ?, version = version + 1 WHERE ref_name = ? AND commit_id = ? AND version = ?", input.Next.CommitID, input.Next.SourceSHA, input.Key.RefName, actual.CommitID, actual.Version)
			if err != nil {
				return localError("fast-forward landing recovery context ref", err)
			}
			changed, _ := result.RowsAffected()
			if changed != 1 {
				return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing recovery context ref compare-and-swap failed"))
			}
		}
		result, err := conn.ExecContext(ctx, "UPDATE working_sets SET base_context_commit_id = ? WHERE worktree_id = ? AND ref_name = ? AND source_sha = ?", input.Next.CommitID, input.Key.WorktreeID, input.Key.RefName, input.Key.SourceSHA)
		if err != nil {
			return localError("advance landing recovery working set parent", err)
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return domain.NewError(domain.CodeStaleWorkingSet, errors.New("working set changed while preparing landing recovery"))
		}
		return s.rebuildSearch(ctx, conn, input.Key)
	})
}

func insertContextChain(ctx context.Context, conn *sql.Conn, chain []storedContextObject) error {
	for _, item := range chain {
		object := item.Object
		parentID := contextObjectPrimaryParentID(object)
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO context_commits (commit_id, parent_id, ref_name, source_sha, message, author, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, item.ID, parentID, object.RefName, object.SourceSHA, object.Message, object.Author, object.CreatedAt.UnixNano()); err != nil {
			return localError("materialize pulled context commit", err)
		}
		if err := insertContextCommitParents(ctx, conn, item.ID, contextObjectParentIDs(object)); err != nil {
			return err
		}
		for _, note := range object.Notes {
			topics, err := encodeNoteTopics(note.Topics)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO committed_notes (context_commit_id, note_id, revision_id, supersedes_revision_id, entity_key, kind, body, author, origin, created_at, binding_state, binding_source_sha, review_reason, topics_json)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item.ID, note.ID, note.RevisionID, note.SupersedesRevisionID, note.EntityKey, note.Kind, note.Body, note.Author, note.Origin, note.CreatedAt.UnixNano(), note.BindingState, note.BindingSourceSHA, note.ReviewReason, topics); err != nil {
				return localError("materialize pulled context note", err)
			}
		}
	}
	return nil
}

func currentContextRef(ctx context.Context, conn *sql.Conn, refName string) (domain.ContextRef, error) {
	ref := domain.ContextRef{RefName: refName}
	err := conn.QueryRowContext(ctx, "SELECT commit_id, source_sha, version FROM context_refs WHERE ref_name = ?", refName).Scan(&ref.CommitID, &ref.SourceSHA, &ref.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, nil
	}
	if err != nil {
		return domain.ContextRef{}, localError("read context ref", err)
	}
	return ref, nil
}

func (s *Store) AddRemote(ctx context.Context, remote domain.Remote) (domain.Remote, error) {
	name, err := domain.NormalizeRemoteName(remote.Name)
	if err != nil {
		return domain.Remote{}, err
	}
	if strings.TrimSpace(remote.Path) == "" {
		return domain.Remote{}, domain.NewError(domain.CodeValidation, errors.New("remote path must not be empty"))
	}
	remote = domain.Remote{Name: name, Path: remote.Path}
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		var existing string
		err := conn.QueryRowContext(ctx, "SELECT path FROM remotes WHERE remote_name = ?", remote.Name).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := conn.ExecContext(ctx, "INSERT INTO remotes (remote_name, path) VALUES (?, ?)", remote.Name, remote.Path); err != nil {
				return localError("add remote", err)
			}
			return nil
		}
		if err != nil {
			return localError("read remote", err)
		}
		if existing != remote.Path {
			return domain.NewError(domain.CodeValidation, fmt.Errorf("remote %q is already configured with a different path", remote.Name))
		}
		return nil
	})
	if err != nil {
		return domain.Remote{}, err
	}
	return remote, nil
}

func (s *Store) Remotes(ctx context.Context) ([]domain.Remote, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT remote_name, path FROM remotes ORDER BY remote_name")
	if err != nil {
		return nil, localError("list remotes", err)
	}
	defer rows.Close()
	var remotes []domain.Remote
	for rows.Next() {
		var remote domain.Remote
		if err := rows.Scan(&remote.Name, &remote.Path); err != nil {
			return nil, localError("scan remote", err)
		}
		remotes = append(remotes, remote)
	}
	if err := rows.Err(); err != nil {
		return nil, localError("iterate remotes", err)
	}
	return remotes, nil
}

func (s *Store) Remote(ctx context.Context, name string) (domain.Remote, error) {
	name, err := domain.NormalizeRemoteName(name)
	if err != nil {
		return domain.Remote{}, err
	}
	var remote domain.Remote
	err = s.db.QueryRowContext(ctx, "SELECT remote_name, path FROM remotes WHERE remote_name = ?", name).Scan(&remote.Name, &remote.Path)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Remote{}, domain.NewError(domain.CodeValidation, fmt.Errorf("remote %q is not configured", name))
	}
	if err != nil {
		return domain.Remote{}, localError("read remote", err)
	}
	return remote, nil
}

func (s *Store) RecordRemoteRef(ctx context.Context, ref domain.RemoteRef) error {
	name, err := domain.NormalizeRemoteName(ref.RemoteName)
	if err != nil {
		return err
	}
	if ref.RefName == "" || ref.SourceSHA == "" || ref.Version < 1 {
		return domain.NewError(domain.CodeValidation, errors.New("remote tracking ref is incomplete"))
	}
	commitID, err := domain.NormalizeContextCommitID(ref.CommitID)
	if err != nil {
		return err
	}
	ref.RemoteName = name
	ref.CommitID = commitID
	return s.withImmediate(ctx, func(conn *sql.Conn) error {
		var existing domain.RemoteRef
		err := conn.QueryRowContext(ctx, "SELECT remote_name, ref_name, commit_id, source_sha, version FROM remote_refs WHERE remote_name = ? AND ref_name = ?", ref.RemoteName, ref.RefName).Scan(&existing.RemoteName, &existing.RefName, &existing.CommitID, &existing.SourceSHA, &existing.Version)
		if err == nil && existing.Version > ref.Version {
			return domain.NewError(domain.CodeRemoteConflict, errors.New("remote tracking ref would move backward"))
		}
		if err == nil && existing.Version == ref.Version && (existing.CommitID != ref.CommitID || existing.SourceSHA != ref.SourceSHA) {
			return domain.NewError(domain.CodeRemoteConflict, errors.New("remote tracking ref version has different contents"))
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return localError("read remote tracking ref", err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO remote_refs (remote_name, ref_name, commit_id, source_sha, version)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(remote_name, ref_name) DO UPDATE SET commit_id = excluded.commit_id, source_sha = excluded.source_sha, version = excluded.version`, ref.RemoteName, ref.RefName, ref.CommitID, ref.SourceSHA, ref.Version); err != nil {
			return localError("record remote tracking ref", err)
		}
		return nil
	})
}
