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

func TestRunExtractsScopedRustEntities(t *testing.T) {
	root := t.TempDir()
	contents := `pub struct Service;
pub struct Generic<T>(T);
pub union Value { pub number: u32 }
pub enum Mode { Fast }
pub trait Runner {
    type Output;
    fn execute(&self) -> Self::Output;
}
impl Service {
    pub fn run(&self) {}
    pub fn outer() {
        fn nested() {}
    }
}
impl<T> Generic<T> {
    pub fn get(&self) {}
}
impl Runner for Service {
    type Output = String;
    fn execute(&self) -> Self::Output { String::new() }
}
pub type Alias = Service;
pub mod tools {
    pub fn helper() {}
}
macro_rules! generated { () => { fn hidden() {} } }
`
	path := filepath.Join(root, "src", "lib.rs")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"rust","files":["src/lib.rs"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]string{
		"Alias":                "type",
		"Generic":              "type",
		"Generic.get":          "method",
		"Mode":                 "enum",
		"Runner":               "interface",
		"Runner.Output":        "type",
		"Runner.execute":       "method",
		"Service":              "type",
		"Service.Output":       "type",
		"Service.execute":      "method",
		"Service.outer":        "method",
		"Service.outer.nested": "function",
		"Service.run":          "method",
		"Value":                "type",
		"tools.helper":         "function",
	}
	if got.Indexer.ID != "thread-keep-index-rust" || got.Language != "rust" || len(got.Entities) != len(want) {
		t.Fatalf("response = %#v, want Rust entities %#v", got, want)
	}
	for _, entity := range got.Entities {
		if kind, found := want[entity.QualifiedName]; !found || entity.Kind != kind {
			t.Fatalf("unexpected entity = %#v", entity)
		}
	}
}

func TestRunRejectsInvalidRustRequestsAndSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "lib.rs"), []byte("pub struct App;\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(lib.rs): %v", err)
	}
	valid := `{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"rust","files":["lib.rs"]}`
	for name, input := range map[string]string{
		"wrong language": strings.Replace(valid, `"rust"`, `"java"`, 1),
		"trailing value": valid + ` {}`,
		"unsafe path":    strings.Replace(valid, `"lib.rs"`, `"../lib.rs"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
				t.Fatal("run() error = nil, want request rejection")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "Broken.rs"), []byte("pub fn broken( {"), 0o644); err != nil {
		t.Fatalf("WriteFile(Broken.rs): %v", err)
	}
	broken := strings.Replace(valid, `"lib.rs"`, `"Broken.rs"`, 1)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(broken), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want syntax rejection")
	}
}

func TestRunRejectsSourceSymlinkOutsideRepository(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "Outside.rs")
	if err := os.WriteFile(external, []byte("pub struct Outside;\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(external): %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "Linked.rs")); err != nil {
		t.Fatalf("Symlink(): %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"rust","files":["Linked.rs"]}`)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want source symlink rejection")
	}
}
