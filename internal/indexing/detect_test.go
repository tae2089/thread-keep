package indexing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDetectContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := DetectContext(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DetectContext() error = %v, want context canceled", err)
	}
}

func TestDetectCandidatesGroupsKnownLanguagesAndSkipsIgnoredDirectories(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(external, []byte("package outside"), 0o644); err != nil {
		t.Fatalf("WriteFile(external): %v", err)
	}
	for path := range map[string]struct{}{
		"cmd/main.go":                    {},
		"web/src/app.ts":                 {},
		"web/src/view.tsx":               {},
		"web/src/legacy.js":              {},
		"web/src/view.jsx":               {},
		"web/src/module.mjs":             {},
		"web/src/common.cjs":             {},
		"services/api.py":                {},
		"services/types.pyi":             {},
		"scripts/window.pyw":             {},
		"src/Main.java":                  {},
		"src/App.kt":                     {},
		"scripts/build.kts":              {},
		"crates/core/src/lib.rs":         {},
		"vendor/ignored.go":              {},
		"node_modules/ignored/index.ts":  {},
		"node_modules/ignored/index.js":  {},
		"node_modules/ignored/index.py":  {},
		"node_modules/ignored/Main.java": {},
		"node_modules/ignored/App.kt":    {},
		"node_modules/ignored/lib.rs":    {},
		".git/ignored.go":                {},
		"README.md":                      {},
	} {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", path, err)
		}
		if err := os.WriteFile(fullPath, []byte("source"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", path, err)
		}
	}
	if err := os.Symlink(external, filepath.Join(root, "linked.go")); err != nil {
		t.Fatalf("Symlink(): %v", err)
	}

	candidates, err := Detect(root)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	want := []Candidate{
		{Language: Go, Files: []string{"cmd/main.go"}},
		{Language: TypeScript, Files: []string{"web/src/app.ts", "web/src/view.tsx"}},
		{Language: JavaScript, Files: []string{"web/src/common.cjs", "web/src/legacy.js", "web/src/module.mjs", "web/src/view.jsx"}},
		{Language: Python, Files: []string{"scripts/window.pyw", "services/api.py", "services/types.pyi"}},
		{Language: Java, Files: []string{"src/Main.java"}},
		{Language: Kotlin, Files: []string{"scripts/build.kts", "src/App.kt"}},
		{Language: Rust, Files: []string{"crates/core/src/lib.rs"}},
	}
	if !reflect.DeepEqual(candidates, want) {
		t.Fatalf("Detect() = %#v, want %#v", candidates, want)
	}
}
