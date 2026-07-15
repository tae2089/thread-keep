package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tae2089/thread-keep/internal/domain"
)

type Layout struct {
	Root       string
	Database   string
	ObjectDir  string
	selfIgnore bool
}

type Store struct {
	db     *sql.DB
	layout Layout
}

func NewLayout(worktreeRoot string) Layout {
	return newLayout(filepath.Join(worktreeRoot, ".thread-keep"), true)
}

func ValidateObjectChain(layout Layout, commitID, repositoryID, refName string) error {
	_, err := ReadContextObject(layout, commitID, repositoryID, refName)
	return err
}

func Open(ctx context.Context, layout Layout) (*Store, error) {
	if err := os.MkdirAll(layout.Root, 0o755); err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create storage directory: %w", err))
	}
	if layout.selfIgnore {
		if err := ensureSelfIgnored(layout.Root); err != nil {
			return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("ignore storage directory: %w", err))
		}
	}
	if err := os.MkdirAll(layout.ObjectDir, 0o755); err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create object directory: %w", err))
	}
	database, err := sql.Open("sqlite3", layout.Database+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("open SQLite database: %w", err))
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	store := &Store{db: database, layout: layout}
	if err := store.migrate(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func newLayout(root string, selfIgnore bool) Layout {
	return Layout{Root: root, Database: filepath.Join(root, "index.sqlite"), ObjectDir: filepath.Join(root, "objects"), selfIgnore: selfIgnore}
}
