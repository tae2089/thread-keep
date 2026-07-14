package indexing

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestListReportsBuiltinMissingAndInstalledPacksWithoutExecution(t *testing.T) {
	configDir := testUserConfigDir(t)
	root := t.TempDir()
	for path := range map[string]struct{}{"main.go": {}, "web/app.ts": {}, "web/app.js": {}, "services/app.py": {}, "src/Main.java": {}, "src/App.kt": {}, "crates/core/src/lib.rs": {}} {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
		if err := os.WriteFile(fullPath, []byte("source"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}

	statuses, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	wantMissing := []domain.IndexerStatus{
		{Language: "go", PackID: "builtin/go", State: domain.IndexerBuiltin, Detected: true},
		{Language: "typescript", PackID: "thread-keep-index-typescript", State: domain.IndexerMissing, Detected: true},
		{Language: "javascript", PackID: "thread-keep-index-javascript", State: domain.IndexerMissing, Detected: true},
		{Language: "python", PackID: "thread-keep-index-python", State: domain.IndexerMissing, Detected: true},
		{Language: "java", PackID: "thread-keep-index-java", State: domain.IndexerMissing, Detected: true},
		{Language: "kotlin", PackID: "thread-keep-index-kotlin", State: domain.IndexerMissing, Detected: true},
		{Language: "rust", PackID: "thread-keep-index-rust", State: domain.IndexerMissing, Detected: true},
	}
	if !reflect.DeepEqual(statuses, wantMissing) {
		t.Fatalf("List() without pack = %#v, want %#v", statuses, wantMissing)
	}

	packPath := filepath.Join(configDir, "thread-keep", "packs", "thread-keep-index-typescript")
	javaScriptPackPath := filepath.Join(configDir, "thread-keep", "packs", "thread-keep-index-javascript")
	pythonPackPath := filepath.Join(configDir, "thread-keep", "packs", "thread-keep-index-python")
	javaPackPath := filepath.Join(configDir, "thread-keep", "packs", "thread-keep-index-java")
	kotlinPackPath := filepath.Join(configDir, "thread-keep", "packs", "thread-keep-index-kotlin")
	rustPackPath := filepath.Join(configDir, "thread-keep", "packs", "thread-keep-index-rust")
	marker := filepath.Join(root, "pack-was-executed")
	if err := os.MkdirAll(filepath.Dir(packPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(pack directory): %v", err)
	}
	if err := os.WriteFile(packPath, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(pack): %v", err)
	}
	if err := os.WriteFile(javaScriptPackPath, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(JavaScript pack): %v", err)
	}
	if err := os.WriteFile(pythonPackPath, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(Python pack): %v", err)
	}
	if err := os.WriteFile(javaPackPath, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(Java pack): %v", err)
	}
	if err := os.WriteFile(kotlinPackPath, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(Kotlin pack): %v", err)
	}
	if err := os.WriteFile(rustPackPath, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(Rust pack): %v", err)
	}

	statuses, err = List(context.Background(), root)
	if err != nil {
		t.Fatalf("List() with pack error = %v", err)
	}
	wantInstalled := []domain.IndexerStatus{
		{Language: "go", PackID: "builtin/go", State: domain.IndexerBuiltin, Detected: true},
		{Language: "typescript", PackID: "thread-keep-index-typescript", State: domain.IndexerInstalled, Detected: true, Path: packPath},
		{Language: "javascript", PackID: "thread-keep-index-javascript", State: domain.IndexerInstalled, Detected: true, Path: javaScriptPackPath},
		{Language: "python", PackID: "thread-keep-index-python", State: domain.IndexerInstalled, Detected: true, Path: pythonPackPath},
		{Language: "java", PackID: "thread-keep-index-java", State: domain.IndexerInstalled, Detected: true, Path: javaPackPath},
		{Language: "kotlin", PackID: "thread-keep-index-kotlin", State: domain.IndexerInstalled, Detected: true, Path: kotlinPackPath},
		{Language: "rust", PackID: "thread-keep-index-rust", State: domain.IndexerInstalled, Detected: true, Path: rustPackPath},
	}
	if !reflect.DeepEqual(statuses, wantInstalled) {
		t.Fatalf("List() with pack = %#v, want %#v", statuses, wantInstalled)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("List() executed pack, marker stat error = %v", err)
	}
}

func TestListTreatsDirectoryAndNonExecutablePackAsMissing(t *testing.T) {
	configDir := testUserConfigDir(t)
	root := t.TempDir()
	packPath := filepath.Join(configDir, "thread-keep", "packs", "thread-keep-index-typescript")
	if err := os.MkdirAll(packPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(pack path): %v", err)
	}
	statuses, err := List(context.Background(), root)
	if err != nil {
		t.Fatalf("List() with pack directory error = %v", err)
	}
	if statuses[1].State != domain.IndexerMissing {
		t.Fatalf("directory status = %#v, want missing", statuses[1])
	}
	if err := os.Remove(packPath); err != nil {
		t.Fatalf("Remove(pack path): %v", err)
	}
	if err := os.WriteFile(packPath, []byte("not executable"), 0o644); err != nil {
		t.Fatalf("WriteFile(non-executable pack): %v", err)
	}
	statuses, err = List(context.Background(), root)
	if err != nil {
		t.Fatalf("List() with non-executable pack error = %v", err)
	}
	if statuses[1].State != domain.IndexerMissing {
		t.Fatalf("non-executable status = %#v, want missing", statuses[1])
	}
}

func TestListReturnsNoPartialResultWhenConfigurationLookupFails(t *testing.T) {
	t.Setenv("APPDATA", "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	statuses, err := List(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("List() error = nil, want user configuration lookup failure")
	}
	if statuses != nil {
		t.Fatalf("List() statuses = %#v, want no partial result", statuses)
	}
}

func testUserConfigDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() error = %v", err)
	}
	return configDir
}

func TestProcessIndexerNormalizesOnlyAllowedEntities(t *testing.T) {
	root := t.TempDir()
	pack := filepath.Join(root, "thread-keep-index-typescript")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"protocol_version\":1,\"indexer\":{\"id\":\"thread-keep-index-typescript\",\"version\":\"1\"},\"language\":\"typescript\",\"entities\":[{\"path\":\"web/app.ts\",\"kind\":\"function\",\"name\":\"run\",\"qualified_name\":\"run\",\"signature\":\"function run()\",\"start_line\":1,\"end_line\":1,\"structural_hash\":\"abc\"}],\"diagnostics\":[]}'\n"
	if err := os.WriteFile(pack, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pack: %v", err)
	}
	result, err := (ProcessIndexer{language: TypeScript, path: pack}).Index(context.Background(), Request{RepositoryRoot: root, SourceSHA: "sha", Language: TypeScript, Files: []string{"web/app.ts"}})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if result.Indexer.ID != "thread-keep-index-typescript" || len(result.Entities) != 1 || result.Entities[0].Key != "typescript:web/app.ts#function:run" {
		t.Fatalf("Index() = %#v, want normalized TypeScript entity", result)
	}
}

func TestProcessIndexerRejectsUnrequestedPath(t *testing.T) {
	root := t.TempDir()
	pack := filepath.Join(root, "thread-keep-index-typescript")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"protocol_version\":1,\"indexer\":{\"id\":\"thread-keep-index-typescript\",\"version\":\"1\"},\"language\":\"typescript\",\"entities\":[{\"path\":\"other.ts\",\"kind\":\"function\",\"name\":\"run\",\"qualified_name\":\"run\",\"start_line\":1,\"end_line\":1,\"structural_hash\":\"abc\"}]}'\n"
	if err := os.WriteFile(pack, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pack: %v", err)
	}
	_, err := (ProcessIndexer{language: TypeScript, path: pack}).Index(context.Background(), Request{RepositoryRoot: root, SourceSHA: "sha", Language: TypeScript, Files: []string{"web/app.ts"}})
	if err == nil {
		t.Fatal("Index() error = nil, want unrequested path rejection")
	}
}

func TestProcessIndexerRejectsProtocolV2Response(t *testing.T) {
	root := t.TempDir()
	pack := filepath.Join(root, "thread-keep-index-typescript")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"protocol_version\":2,\"indexer\":{\"id\":\"thread-keep-index-typescript\",\"version\":\"2\"},\"language\":\"typescript\",\"entities\":[]}'\n"
	if err := os.WriteFile(pack, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pack: %v", err)
	}

	_, err := (ProcessIndexer{language: TypeScript, path: pack}).Index(context.Background(), Request{RepositoryRoot: root, SourceSHA: "sha", Language: TypeScript})
	if err == nil {
		t.Fatal("Index() error = nil, want protocol v2 rejection")
	}
}

func TestProcessIndexerRejectsPackVersionMismatch(t *testing.T) {
	root := t.TempDir()
	pack := filepath.Join(root, "thread-keep-index-typescript")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"protocol_version\":1,\"indexer\":{\"id\":\"thread-keep-index-typescript\",\"version\":\"2.0.0\"},\"language\":\"typescript\",\"entities\":[]}'\n"
	if err := os.WriteFile(pack, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pack: %v", err)
	}
	indexer := ProcessIndexer{language: TypeScript, path: pack, descriptor: Descriptor{ID: packID(TypeScript), Version: "1.0.0"}}
	if _, err := indexer.Index(context.Background(), Request{RepositoryRoot: root, SourceSHA: "sha", Language: TypeScript}); err == nil {
		t.Fatal("Index() error = nil, want managed version mismatch")
	}
}

func TestResolveAvailablePackFindsPyPIPack(t *testing.T) {
	configDir := t.TempDir()
	packPath := filepath.Join(t.TempDir(), packExecutableName(packID(TypeScript)))
	if err := os.WriteFile(packPath, []byte("pack"), 0o755); err != nil {
		t.Fatalf("WriteFile(PyPI pack): %v", err)
	}

	pack, found, err := resolveAvailablePack(configDir, pypiPackJSON(t, TypeScript, packPath, "1.2.3"), "1.2.3", TypeScript)
	if err != nil {
		t.Fatalf("resolveAvailablePack() error = %v", err)
	}
	if !found || pack.Path != packPath || pack.Descriptor.ID != packID(TypeScript) || pack.Descriptor.Version != "1.2.3" {
		t.Fatalf("resolveAvailablePack() = %#v, %v, want PyPI pack", pack, found)
	}
}

func TestResolveAvailablePackPrefersPyPIPackOverLocalPack(t *testing.T) {
	configDir := t.TempDir()
	localPath := filepath.Join(packDirectory(configDir), packExecutableName(packID(TypeScript)))
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(local pack directory): %v", err)
	}
	if err := os.WriteFile(localPath, []byte("local"), 0o755); err != nil {
		t.Fatalf("WriteFile(local pack): %v", err)
	}
	pypiPath := filepath.Join(t.TempDir(), packExecutableName(packID(TypeScript)))
	if err := os.WriteFile(pypiPath, []byte("pypi"), 0o755); err != nil {
		t.Fatalf("WriteFile(PyPI pack): %v", err)
	}

	pack, found, err := resolveAvailablePack(configDir, pypiPackJSON(t, TypeScript, pypiPath, "1.2.3"), "1.2.3", TypeScript)
	if err != nil {
		t.Fatalf("resolveAvailablePack() error = %v", err)
	}
	if !found || pack.Path != pypiPath || pack.Descriptor.Version != "1.2.3" {
		t.Fatalf("resolveAvailablePack() = %#v, %v, want PyPI pack", pack, found)
	}
}

func TestResolveAvailablePackRejectsInvalidPyPIPackContract(t *testing.T) {
	packPath := filepath.Join(t.TempDir(), packExecutableName(packID(TypeScript)))
	if err := os.WriteFile(packPath, []byte("pack"), 0o755); err != nil {
		t.Fatalf("WriteFile(PyPI pack): %v", err)
	}

	tests := map[string]struct {
		mapping string
		version string
	}{
		"malformed JSON":   {mapping: "{"},
		"invalid version":  {mapping: pypiPackJSON(t, TypeScript, packPath, "latest"), version: "latest"},
		"version mismatch": {mapping: pypiPackJSON(t, TypeScript, packPath, "1.2.2"), version: "1.2.3"},
		"unknown language": {mapping: `{"ruby":{"path":"/tmp/thread-keep-index-ruby","version":"1.2.3"}}`, version: "1.2.3"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := resolveAvailablePack(t.TempDir(), test.mapping, test.version, TypeScript); domain.CodeOf(err) != domain.CodeValidation {
				t.Fatalf("resolveAvailablePack() error = %v, want validation", err)
			}
		})
	}
}

func pypiPackJSON(t *testing.T, language Language, path, version string) string {
	t.Helper()
	value, err := json.Marshal(map[Language]map[string]string{
		language: {"path": path, "version": version},
	})
	if err != nil {
		t.Fatalf("Marshal(PyPI pack mapping): %v", err)
	}
	return string(value)
}
