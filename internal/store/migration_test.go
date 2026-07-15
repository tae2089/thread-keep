package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestNewLayoutUsesSelfIgnoredWorktreeDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	worktree := t.TempDir()
	layout := NewLayout(worktree)
	if want := filepath.Join(worktree, ".thread-keep"); layout.Root != want {
		t.Fatalf("NewLayout().Root = %q, want %q", layout.Root, want)
	}
	contextStore, err := Open(ctx, layout)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := contextStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(layout.Root, ".gitignore"))
	if err != nil {
		t.Fatalf("ReadFile(.gitignore) error = %v", err)
	}
	if string(contents) != "*\n" {
		t.Fatalf(".gitignore = %q, want %q", contents, "*\\n")
	}
}

func TestMigrateLegacyPreservesDatabaseAndObjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	legacy := LegacyLayout(filepath.Join(t.TempDir(), ".git"))
	current := NewLayout(t.TempDir())
	legacyStore, err := Open(ctx, legacy)
	if err != nil {
		t.Fatalf("Open(legacy) error = %v", err)
	}
	if _, err := legacyStore.db.ExecContext(ctx, "CREATE TABLE migration_marker (value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create migration marker: %v", err)
	}
	if _, err := legacyStore.db.ExecContext(ctx, "INSERT INTO migration_marker (value) VALUES ('preserved')"); err != nil {
		t.Fatalf("insert migration marker: %v", err)
	}
	objectName := "0123456789abcdef.json"
	objectBody := []byte(`{"schema_version":3}`)
	if err := os.WriteFile(filepath.Join(legacy.ObjectDir, objectName), objectBody, 0o644); err != nil {
		t.Fatalf("WriteFile(legacy object) error = %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("Close(legacy) error = %v", err)
	}

	migrated, err := MigrateLegacy(ctx, legacy, current)
	if err != nil {
		t.Fatalf("MigrateLegacy() error = %v", err)
	}
	if !migrated {
		t.Fatal("MigrateLegacy() migrated = false, want true")
	}
	if _, err := os.Stat(legacy.Database); err != nil {
		t.Fatalf("legacy database was not preserved: %v", err)
	}
	gotObject, err := os.ReadFile(filepath.Join(current.ObjectDir, objectName))
	if err != nil {
		t.Fatalf("ReadFile(current object) error = %v", err)
	}
	if string(gotObject) != string(objectBody) {
		t.Fatalf("current object = %q, want %q", gotObject, objectBody)
	}
	currentStore, err := Open(ctx, current)
	if err != nil {
		t.Fatalf("Open(current) error = %v", err)
	}
	defer currentStore.Close()
	var marker string
	if err := currentStore.db.QueryRowContext(ctx, "SELECT value FROM migration_marker").Scan(&marker); err != nil {
		t.Fatalf("read migration marker: %v", err)
	}
	if marker != "preserved" {
		t.Fatalf("migration marker = %q, want preserved", marker)
	}
}

func TestMigrateLegacyFailureDoesNotPublishTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	legacy := LegacyLayout(filepath.Join(t.TempDir(), ".git"))
	current := NewLayout(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(legacy.Database), 0o755); err != nil {
		t.Fatalf("MkdirAll(legacy) error = %v", err)
	}
	if err := os.WriteFile(legacy.Database, []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatalf("WriteFile(legacy database) error = %v", err)
	}

	if migrated, err := MigrateLegacy(ctx, legacy, current); domain.CodeOf(err) != domain.CodeLocalStorage || migrated {
		t.Fatalf("MigrateLegacy() = (%t, %v), want false and local_storage", migrated, err)
	}
	if _, err := os.Stat(current.Root); !os.IsNotExist(err) {
		t.Fatalf("current root exists after failed migration: %v", err)
	}
	contents, err := os.ReadFile(legacy.Database)
	if err != nil {
		t.Fatalf("ReadFile(legacy database) error = %v", err)
	}
	if string(contents) != "not a sqlite database" {
		t.Fatalf("legacy database changed to %q", contents)
	}
}

func TestMigrateLegacyReadsClosedWALDatabaseFromReadOnlyDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce POSIX directory write bits")
	}
	t.Parallel()
	ctx := context.Background()
	legacy := LegacyLayout(filepath.Join(t.TempDir(), ".git"))
	current := NewLayout(t.TempDir())
	legacyStore, err := Open(ctx, legacy)
	if err != nil {
		t.Fatalf("Open(legacy) error = %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("Close(legacy) error = %v", err)
	}
	if _, err := os.Stat(legacy.Database + "-wal"); !os.IsNotExist(err) {
		t.Fatalf("legacy WAL exists after close: %v", err)
	}
	if err := os.Chmod(legacy.Database, 0o444); err != nil {
		t.Fatalf("Chmod(legacy database) error = %v", err)
	}
	if err := os.Chmod(legacy.Root, 0o555); err != nil {
		t.Fatalf("Chmod(legacy root) error = %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(legacy.Root, 0o755)
		_ = os.Chmod(legacy.Database, 0o644)
	})

	migrated, err := MigrateLegacy(ctx, legacy, current)
	if err != nil {
		t.Fatalf("MigrateLegacy() error = %v", err)
	}
	if !migrated {
		t.Fatal("MigrateLegacy() migrated = false, want true")
	}
}

func TestMigrateLegacyDoesNotOverwritePartialCurrentRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	legacy := LegacyLayout(filepath.Join(t.TempDir(), ".git"))
	current := NewLayout(t.TempDir())
	legacyStore, err := Open(ctx, legacy)
	if err != nil {
		t.Fatalf("Open(legacy) error = %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("Close(legacy) error = %v", err)
	}
	if err := os.MkdirAll(current.Root, 0o755); err != nil {
		t.Fatalf("MkdirAll(current) error = %v", err)
	}
	marker := filepath.Join(current.Root, "keep-me")
	if err := os.WriteFile(marker, []byte("current"), 0o644); err != nil {
		t.Fatalf("WriteFile(current marker) error = %v", err)
	}

	if migrated, err := MigrateLegacy(ctx, legacy, current); domain.CodeOf(err) != domain.CodeLocalStorage || migrated {
		t.Fatalf("MigrateLegacy() = (%t, %v), want false and local_storage", migrated, err)
	}
	contents, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("ReadFile(current marker) error = %v", err)
	}
	if string(contents) != "current" {
		t.Fatalf("current marker changed to %q", contents)
	}
}

func TestMigrateLegacyKeepsExistingCurrentDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	legacy := LegacyLayout(filepath.Join(t.TempDir(), ".git"))
	current := NewLayout(t.TempDir())
	for _, fixture := range []struct {
		layout Layout
		value  string
	}{
		{layout: legacy, value: "legacy"},
		{layout: current, value: "current"},
	} {
		contextStore, err := Open(ctx, fixture.layout)
		if err != nil {
			t.Fatalf("Open(%s) error = %v", fixture.value, err)
		}
		if _, err := contextStore.db.ExecContext(ctx, "CREATE TABLE migration_precedence (value TEXT NOT NULL)"); err != nil {
			t.Fatalf("create %s marker: %v", fixture.value, err)
		}
		if _, err := contextStore.db.ExecContext(ctx, "INSERT INTO migration_precedence (value) VALUES (?)", fixture.value); err != nil {
			t.Fatalf("insert %s marker: %v", fixture.value, err)
		}
		if err := contextStore.Close(); err != nil {
			t.Fatalf("Close(%s) error = %v", fixture.value, err)
		}
	}

	migrated, err := MigrateLegacy(ctx, legacy, current)
	if err != nil {
		t.Fatalf("MigrateLegacy() error = %v", err)
	}
	if migrated {
		t.Fatal("MigrateLegacy() migrated = true, want existing current database to win")
	}
	currentStore, err := Open(ctx, current)
	if err != nil {
		t.Fatalf("Open(current) error = %v", err)
	}
	defer currentStore.Close()
	var marker string
	if err := currentStore.db.QueryRowContext(ctx, "SELECT value FROM migration_precedence").Scan(&marker); err != nil {
		t.Fatalf("read current marker: %v", err)
	}
	if marker != "current" {
		t.Fatalf("current marker = %q, want current", marker)
	}
}
