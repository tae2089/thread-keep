package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/mattn/go-sqlite3"
	"github.com/tae2089/thread-keep/internal/domain"
)

const selfIgnoreContents = "*\n"

func LegacyLayout(commonGitDir string) Layout {
	return newLayout(filepath.Join(commonGitDir, "thread-keep"), false)
}

func MigrateLegacy(ctx context.Context, legacy, current Layout) (bool, error) {
	currentExists, err := regularFileExists(current.Database)
	if err != nil {
		return false, localMigrationError("inspect current database", err)
	}
	if currentExists {
		return false, nil
	}
	legacyExists, err := regularFileExists(legacy.Database)
	if err != nil {
		return false, localMigrationError("inspect legacy database", err)
	}
	if !legacyExists {
		return false, nil
	}
	if _, err := os.Stat(current.Root); err == nil {
		return false, localMigrationError("prepare current storage", errors.New("storage directory exists without a database"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, localMigrationError("inspect current storage", err)
	}

	stagingRoot, err := os.MkdirTemp(filepath.Dir(current.Root), ".thread-keep-migrate-*")
	if err != nil {
		return false, localMigrationError("create migration staging directory", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stagingRoot)
		}
	}()
	staging := newLayout(stagingRoot, true)
	if err := ensureSelfIgnored(staging.Root); err != nil {
		return false, localMigrationError("ignore migration staging directory", err)
	}
	if err := os.MkdirAll(staging.ObjectDir, 0o755); err != nil {
		return false, localMigrationError("create staged object directory", err)
	}
	if err := backupSQLite(ctx, legacy.Database, staging.Database); err != nil {
		return false, localMigrationError("backup legacy database", err)
	}
	if err := copyObjectFiles(legacy.ObjectDir, staging.ObjectDir); err != nil {
		return false, localMigrationError("copy legacy objects", err)
	}
	if err := validateSQLite(ctx, staging.Database); err != nil {
		return false, localMigrationError("validate migrated database", err)
	}
	if err := os.Rename(staging.Root, current.Root); err != nil {
		return false, localMigrationError("publish migrated storage", err)
	}
	published = true
	return true, nil
}

func ensureSelfIgnored(root string) error {
	path := filepath.Join(root, ".gitignore")
	contents, err := os.ReadFile(path)
	if err == nil {
		if string(contents) != selfIgnoreContents {
			return errors.New("storage .gitignore has unexpected contents")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(path, []byte(selfIgnoreContents), 0o644); err != nil {
		return err
	}
	return nil
}

func backupSQLite(ctx context.Context, sourcePath, destinationPath string) error {
	sourceDSN, activeWAL, err := migrationSourceSQLiteDSN(sourcePath)
	if err != nil {
		return fmt.Errorf("inspect source database journal: %w", err)
	}
	source, err := sql.Open("sqlite3", sourceDSN)
	if err != nil {
		return fmt.Errorf("open source database: %w", err)
	}
	defer source.Close()
	destination, err := sql.Open("sqlite3", destinationPath+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("open destination database: %w", err)
	}
	defer destination.Close()
	source.SetMaxOpenConns(1)
	destination.SetMaxOpenConns(1)
	if err := source.PingContext(ctx); err != nil {
		if activeWAL {
			return fmt.Errorf("connect to source database with an active WAL; close older Thread Keep processes and retry: %w", err)
		}
		return fmt.Errorf("connect to source database: %w", err)
	}
	if err := destination.PingContext(ctx); err != nil {
		return fmt.Errorf("connect to destination database: %w", err)
	}
	sourceConn, err := source.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire source connection: %w", err)
	}
	defer sourceConn.Close()
	destinationConn, err := destination.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire destination connection: %w", err)
	}
	defer destinationConn.Close()
	if err := destinationConn.Raw(func(destinationDriver any) error {
		destinationSQLite, ok := destinationDriver.(*sqlite3.SQLiteConn)
		if !ok {
			return errors.New("destination is not a SQLite connection")
		}
		return sourceConn.Raw(func(sourceDriver any) error {
			sourceSQLite, ok := sourceDriver.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("source is not a SQLite connection")
			}
			backup, err := destinationSQLite.Backup("main", sourceSQLite, "main")
			if err != nil {
				return err
			}
			done, stepErr := backup.Step(-1)
			finishErr := backup.Finish()
			if stepErr != nil {
				return stepErr
			}
			if finishErr != nil {
				return finishErr
			}
			if !done {
				return errors.New("SQLite backup did not complete")
			}
			return nil
		})
	}); err != nil {
		return err
	}
	return nil
}

func validateSQLite(ctx context.Context, path string) error {
	database, err := sql.Open("sqlite3", readOnlySQLiteDSN(path, false))
	if err != nil {
		return err
	}
	defer database.Close()
	var result string
	if err := database.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite quick check returned %q", result)
	}
	return nil
}

func migrationSourceSQLiteDSN(path string) (string, bool, error) {
	walInfo, err := os.Stat(path + "-wal")
	if errors.Is(err, os.ErrNotExist) {
		return readOnlySQLiteDSN(path, true), false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !walInfo.Mode().IsRegular() {
		return "", false, errors.New("SQLite WAL path is not a regular file")
	}
	if walInfo.Size() == 0 {
		return readOnlySQLiteDSN(path, true), false, nil
	}
	return readOnlySQLiteDSN(path, false), true, nil
}

func readOnlySQLiteDSN(path string, immutable bool) string {
	address := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := address.Query()
	query.Set("_busy_timeout", "5000")
	query.Set("_query_only", "true")
	if immutable {
		query.Set("immutable", "1")
	}
	query.Set("mode", "ro")
	address.RawQuery = query.Encode()
	return address.String()
}

func copyObjectFiles(sourceDir, destinationDir string) error {
	entries, err := os.ReadDir(sourceDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("object %q is not a regular file", entry.Name())
		}
		if err := copyFile(filepath.Join(sourceDir, entry.Name()), filepath.Join(destinationDir, entry.Name()), info.Mode().Perm()); err != nil {
			return fmt.Errorf("copy object %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func copyFile(sourcePath, destinationPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.OpenFile(destinationPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(destination, source); err != nil {
		_ = destination.Close()
		return err
	}
	if err := destination.Sync(); err != nil {
		_ = destination.Close()
		return err
	}
	return destination.Close()
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("path is not a regular file")
	}
	return true, nil
}

func localMigrationError(operation string, err error) error {
	return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("%s: %w", operation, err))
}
