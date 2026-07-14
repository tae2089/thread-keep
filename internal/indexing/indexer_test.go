package indexing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
)

type fixedIndexer struct {
	descriptor Descriptor
	result     Result
	err        error
}

func TestGoIndexerUsesOnlyRequestedFiles(t *testing.T) {
	root := t.TempDir()
	writeIndexSource(t, root, "included.go", "package sample\nfunc Included() {}\n")
	writeIndexSource(t, root, "excluded.go", "package sample\nfunc Excluded() {}\n")

	result, err := (GoIndexer{}).Index(context.Background(), Request{RepositoryRoot: root, SourceSHA: "sha", Language: Go, Files: []string{"included.go"}})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if len(result.Entities) != 1 || result.Entities[0].Name != "Included" {
		t.Fatalf("Index() entities = %#v, want only Included", result.Entities)
	}
}

func TestGoIndexerRejectsSourceSymlinkOutsideRepository(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(external, []byte("package outside\nfunc Outside() {}\n"), 0o644); err != nil {
		t.Fatalf("write external source: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "linked.go")); err != nil {
		t.Fatalf("create source symlink: %v", err)
	}

	_, err := (GoIndexer{}).Index(context.Background(), Request{RepositoryRoot: root, SourceSHA: "sha", Language: Go, Files: []string{"linked.go"}})
	if err == nil {
		t.Fatal("Index() error = nil, want source symlink rejection")
	}
}

func TestCoordinatorMarksInvalidSuccessfulResultAsFailedCoverage(t *testing.T) {
	root := t.TempDir()
	writeIndexSource(t, root, "app.ts", "export function run() {}\n")
	invalid := domain.Entity{Key: "duplicate", Kind: domain.EntityFunction, Name: "run", Path: "app.ts", StartLine: 1, EndLine: 1, SourceSHA: "sha", StructuralHash: "hash"}
	pack := fixedIndexer{descriptor: Descriptor{ID: "thread-keep-index-typescript", Version: "1"}, result: Result{Indexer: Descriptor{ID: "thread-keep-index-typescript", Version: "1"}, Entities: []domain.Entity{invalid, invalid}}}
	coordinator := Coordinator{Go: GoIndexer{}, Packs: map[Language]Indexer{TypeScript: pack}}

	projections, err := coordinator.Index(context.Background(), root, "sha")
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if len(projections) != 1 || projections[0].Coverage.State != domain.CoverageFailed || len(projections[0].Entities) != 0 || !strings.Contains(projections[0].Coverage.Detail, "duplicate") {
		t.Fatalf("Index() projections = %#v, want failed duplicate coverage", projections)
	}
}

func TestNewCoordinatorResolvesInstalledJavaScriptPackAtIndexTime(t *testing.T) {
	configDir := testUserConfigDir(t)
	root := t.TempDir()
	writeIndexSource(t, root, "web/app.js", "export function run() {}\n")
	coordinator := NewCoordinator()
	packPath := filepath.Join(configDir, "thread-keep", "packs", packID(JavaScript))
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(pack directory): %v", err)
	}
	response := `{"protocol_version":1,"indexer":{"id":"thread-keep-index-javascript","version":"1"},"language":"javascript","entities":[{"path":"web/app.js","kind":"function","name":"run","qualified_name":"run","signature":"function run() {}","start_line":1,"end_line":1,"structural_hash":"hash"}],"diagnostics":[]}`
	if err := os.WriteFile(packPath, []byte("#!/bin/sh\nprintf '%s\\n' '"+response+"'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(JavaScript pack): %v", err)
	}

	projections, err := coordinator.Index(context.Background(), root, "sha")
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if len(projections) != 1 || projections[0].Coverage.State != domain.CoverageIndexed || projections[0].Coverage.Language != string(JavaScript) || projections[0].Coverage.IndexerID != packID(JavaScript) || len(projections[0].Entities) != 1 || projections[0].Entities[0].Key != "javascript:web/app.js#function:run" {
		t.Fatalf("Index() projections = %#v, want indexed JavaScript entity", projections)
	}
}

func TestNewCoordinatorUsesInstalledPythonPack(t *testing.T) {
	configDir := testUserConfigDir(t)
	root := t.TempDir()
	writeIndexSource(t, root, "services/app.py", "def run():\n    return None\n")
	packPath := filepath.Join(configDir, "thread-keep", "packs", packID(Python))
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(pack directory): %v", err)
	}
	response := `{"protocol_version":1,"indexer":{"id":"thread-keep-index-python","version":"1"},"language":"python","entities":[{"path":"services/app.py","kind":"function","name":"run","qualified_name":"run","signature":"def run():","start_line":1,"end_line":2,"structural_hash":"hash"}],"diagnostics":[]}`
	if err := os.WriteFile(packPath, []byte("#!/bin/sh\nprintf '%s\\n' '"+response+"'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(Python pack): %v", err)
	}

	projections, err := NewCoordinator().Index(context.Background(), root, "sha")
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if len(projections) != 1 || projections[0].Coverage.State != domain.CoverageIndexed || projections[0].Coverage.Language != string(Python) || projections[0].Coverage.IndexerID != packID(Python) || len(projections[0].Entities) != 1 || projections[0].Entities[0].Key != "python:services/app.py#function:run" {
		t.Fatalf("Index() projections = %#v, want indexed Python entity", projections)
	}
}

func TestNewCoordinatorUsesInstalledJavaPack(t *testing.T) {
	configDir := testUserConfigDir(t)
	root := t.TempDir()
	writeIndexSource(t, root, "src/Main.java", "class Main { void run() {} }\n")
	packPath := filepath.Join(configDir, "thread-keep", "packs", packID(Java))
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(pack directory): %v", err)
	}
	response := `{"protocol_version":1,"indexer":{"id":"thread-keep-index-java","version":"1"},"language":"java","entities":[{"path":"src/Main.java","kind":"class","name":"Main","qualified_name":"Main","signature":"class Main {}","start_line":1,"end_line":1,"structural_hash":"hash"}],"diagnostics":[]}`
	if err := os.WriteFile(packPath, []byte("#!/bin/sh\nprintf '%s\\n' '"+response+"'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(Java pack): %v", err)
	}

	projections, err := NewCoordinator().Index(context.Background(), root, "sha")
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if len(projections) != 1 || projections[0].Coverage.State != domain.CoverageIndexed || projections[0].Coverage.Language != string(Java) || projections[0].Coverage.IndexerID != packID(Java) || len(projections[0].Entities) != 1 || projections[0].Entities[0].Key != "java:src/Main.java#class:Main" {
		t.Fatalf("Index() projections = %#v, want indexed Java entity", projections)
	}
}

func TestNewCoordinatorUsesInstalledKotlinPack(t *testing.T) {
	configDir := testUserConfigDir(t)
	root := t.TempDir()
	writeIndexSource(t, root, "src/App.kt", "class App { fun run() {} }\n")
	packPath := filepath.Join(configDir, "thread-keep", "packs", packID(Kotlin))
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(pack directory): %v", err)
	}
	response := `{"protocol_version":1,"indexer":{"id":"thread-keep-index-kotlin","version":"1"},"language":"kotlin","entities":[{"path":"src/App.kt","kind":"class","name":"App","qualified_name":"App","signature":"class App {}","start_line":1,"end_line":1,"structural_hash":"hash"}],"diagnostics":[]}`
	if err := os.WriteFile(packPath, []byte("#!/bin/sh\nprintf '%s\\n' '"+response+"'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(Kotlin pack): %v", err)
	}

	projections, err := NewCoordinator().Index(context.Background(), root, "sha")
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if len(projections) != 1 || projections[0].Coverage.State != domain.CoverageIndexed || projections[0].Coverage.Language != string(Kotlin) || projections[0].Coverage.IndexerID != packID(Kotlin) || len(projections[0].Entities) != 1 || projections[0].Entities[0].Key != "kotlin:src/App.kt#class:App" {
		t.Fatalf("Index() projections = %#v, want indexed Kotlin entity", projections)
	}
}

func TestNewCoordinatorUsesInstalledRustPack(t *testing.T) {
	configDir := testUserConfigDir(t)
	root := t.TempDir()
	writeIndexSource(t, root, "crates/core/src/lib.rs", "pub struct Service;\n")
	packPath := filepath.Join(configDir, "thread-keep", "packs", packID(Rust))
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(pack directory): %v", err)
	}
	response := `{"protocol_version":1,"indexer":{"id":"thread-keep-index-rust","version":"1"},"language":"rust","entities":[{"path":"crates/core/src/lib.rs","kind":"type","name":"Service","qualified_name":"Service","signature":"pub struct Service;","start_line":1,"end_line":1,"structural_hash":"hash"}],"diagnostics":[]}`
	if err := os.WriteFile(packPath, []byte("#!/bin/sh\nprintf '%s\\n' '"+response+"'\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(Rust pack): %v", err)
	}

	projections, err := NewCoordinator().Index(context.Background(), root, "sha")
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if len(projections) != 1 || projections[0].Coverage.State != domain.CoverageIndexed || projections[0].Coverage.Language != string(Rust) || projections[0].Coverage.IndexerID != packID(Rust) || len(projections[0].Entities) != 1 || projections[0].Entities[0].Key != "rust:crates/core/src/lib.rs#type:Service" {
		t.Fatalf("Index() projections = %#v, want indexed Rust entity", projections)
	}
}

func (f fixedIndexer) Descriptor() Descriptor { return f.descriptor }

func (f fixedIndexer) Index(context.Context, Request) (Result, error) { return f.result, f.err }

func writeIndexSource(t *testing.T, root, relative, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", relative, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", relative, err)
	}
}
