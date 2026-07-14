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

func TestRunExtractsJavaScriptEntitiesAcrossSupportedExtensions(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"src/app.js": `export function run() {}
export function* iterate() {}
export class Service {
  run() {}
  handler = () => {}
  value = 1
}
export const helper = () => {}
`,
		"src/view.jsx":   "export const Widget = () => <section />\n",
		"src/module.mjs": "export function moduleRun() {}\n",
		"src/common.cjs": "function commonRun() {}\n",
	}
	for relative, contents := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", relative, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", relative, err)
		}
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"javascript","files":["src/app.js","src/common.cjs","src/module.mjs","src/view.jsx"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]string{
		"Service":         "class",
		"Service.handler": "method",
		"Service.run":     "method",
		"commonRun":       "function",
		"helper":          "function",
		"iterate":         "function",
		"moduleRun":       "function",
		"run":             "function",
		"Widget":          "function",
	}
	if got.Indexer.ID != "thread-keep-index-javascript" || got.Language != "javascript" || len(got.Entities) != len(want) {
		t.Fatalf("response = %#v, want JavaScript entities %#v", got, want)
	}
	for _, entity := range got.Entities {
		if kind, found := want[entity.QualifiedName]; !found || entity.Kind != kind {
			t.Fatalf("unexpected entity = %#v", entity)
		}
	}
}

func TestRunRejectsInvalidJavaScriptRequestsAndSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("function run() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.js): %v", err)
	}
	valid := `{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"javascript","files":["app.js"]}`
	for name, input := range map[string]string{
		"wrong language": strings.Replace(valid, `"javascript"`, `"typescript"`, 1),
		"trailing value": valid + ` {}`,
		"unsafe path":    strings.Replace(valid, `"app.js"`, `"../app.js"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
				t.Fatal("run() error = nil, want request rejection")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "broken.js"), []byte("function broken( {"), 0o644); err != nil {
		t.Fatalf("WriteFile(broken.js): %v", err)
	}
	broken := strings.Replace(valid, `"app.js"`, `"broken.js"`, 1)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(broken), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want syntax rejection")
	}
}

func TestRunRejectsSourceSymlinkOutsideRepository(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "outside.js")
	if err := os.WriteFile(external, []byte("function outside() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(external): %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "linked.js")); err != nil {
		t.Fatalf("Symlink(): %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"javascript","files":["linked.js"]}`)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want source symlink rejection")
	}
}
