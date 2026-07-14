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
	Root      string
	Database  string
	ObjectDir string
}

type Store struct {
	db     *sql.DB
	layout Layout
}

func NewLayout(commonGitDir string) Layout {
	root := filepath.Join(commonGitDir, "thread-keep")
	return Layout{Root: root, Database: filepath.Join(root, "index.sqlite"), ObjectDir: filepath.Join(root, "objects")}
}

func ValidateObjectChain(layout Layout, commitID, repositoryID, refName string) error {
	_, err := ReadContextObject(layout, commitID, repositoryID, refName)
	return err
}

func Open(ctx context.Context, layout Layout) (*Store, error) {
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
