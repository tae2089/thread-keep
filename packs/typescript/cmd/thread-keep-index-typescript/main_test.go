package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunExtractsTypeScriptEntities(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatalf("make source directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "app.ts"), []byte("export interface User { id: string }\nexport class Service { run(): void {} }\nexport const helper = () => 1\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"typescript","files":["web/app.ts"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Indexer.ID != "thread-keep-index-typescript" || got.Indexer.Version != "dev" || len(got.Entities) != 4 {
		t.Fatalf("response = %#v, want interface, class, method and function", got)
	}
}

func TestRunAcceptsFilenameContainingTwoDots(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "name..part.ts"), []byte("export function run() {}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"typescript","files":["name..part.ts"]}`)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &bytes.Buffer{}); err != nil {
		t.Fatalf("run() error = %v, want valid dotted filename", err)
	}
}

func TestRunRejectsTrailingAndOversizedRequests(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.ts"), []byte("export function run() {}\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	valid := `{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"typescript","files":["app.ts"]}`
	for name, input := range map[string]string{
		"trailing value": valid + ` {}`,
		"oversized":      valid + strings.Repeat(" ", (2<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
				t.Fatal("run() error = nil, want protocol rejection")
			}
		})
	}
}

func TestRunRejectsSyntaxErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "broken.ts"), []byte("export function broken( {"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"typescript","files":["broken.ts"]}`)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want syntax error")
	}
}

func TestRunMergesOverloadsAndExcludesDataFields(t *testing.T) {
	root := t.TempDir()
	source := `export function parse(value: string): string;
export function parse(value: number): number;
export function parse(value: unknown): unknown { return value }
export class Service {
  value: number = 1
  handler = () => {}
  run(value: string): void;
  run(value: number): void;
  run(value: unknown): void {}
}
`
	if err := os.WriteFile(filepath.Join(root, "overloads.ts"), []byte(source), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"typescript","files":["overloads.ts"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]string{"parse": "function", "Service": "class", "Service.handler": "method", "Service.run": "method"}
	if len(got.Entities) != len(want) {
		t.Fatalf("entities = %#v, want one logical entity per overload set", got.Entities)
	}
	for _, entity := range got.Entities {
		if kind, found := want[entity.QualifiedName]; !found || entity.Kind != kind {
			t.Fatalf("unexpected entity = %#v", entity)
		}
	}
}

func TestRunRejectsSourceSymlinkOutsideRepository(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "outside.ts")
	if err := os.WriteFile(external, []byte("export function outside() {}\n"), 0o644); err != nil {
		t.Fatalf("write external source: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "linked.ts")); err != nil {
		t.Fatalf("create source symlink: %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"typescript","files":["linked.ts"]}`)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want source symlink rejection")
	}
}

func TestRunKeepsSameNamedFunctionsInDifferentNamespacesDistinct(t *testing.T) {
	root := t.TempDir()
	source := "namespace Alpha { export function run() { function inner() {} } }\nnamespace Beta { export function run() {} }\n"
	if err := os.WriteFile(filepath.Join(root, "namespaces.ts"), []byte(source), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"typescript","files":["namespaces.ts"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]bool{"Alpha.run": true, "Alpha.run.inner": true, "Beta.run": true}
	if len(got.Entities) != len(want) {
		t.Fatalf("entities = %#v, want namespace-qualified functions", got.Entities)
	}
	for _, entity := range got.Entities {
		if !want[entity.QualifiedName] {
			t.Fatalf("unexpected qualified name %q in %#v", entity.QualifiedName, got.Entities)
		}
	}
}
