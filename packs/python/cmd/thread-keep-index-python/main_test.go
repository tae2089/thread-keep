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

func TestRunExtractsPythonEntitiesAcrossSupportedExtensions(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"src/app.py": `def run():
    return None

async def fetch():
    return None

@trace
def decorated():
    return None

class Service:
    value = 1
    callback = lambda: None

    def run(self):
        return None

    async def fetch(self):
        return None

    @classmethod
    def create(cls):
        return cls()

    def outer(self):
        def nested():
            return None
        return nested

def outer():
    class Local:
        def call(self):
            return None
    return Local
`,
		"src/types.pyi": `class Stub:
    def run(self) -> None: ...
`,
		"src/window.pyw": "def start_window():\n    return None\n",
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
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"python","files":["src/app.py","src/types.pyi","src/window.pyw"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]string{
		"Service":              "class",
		"Service.create":       "method",
		"Service.fetch":        "method",
		"Service.outer":        "method",
		"Service.outer.nested": "function",
		"Service.run":          "method",
		"Stub":                 "class",
		"Stub.run":             "method",
		"decorated":            "function",
		"fetch":                "function",
		"outer":                "function",
		"outer.Local":          "class",
		"outer.Local.call":     "method",
		"run":                  "function",
		"start_window":         "function",
	}
	if got.Indexer.ID != "thread-keep-index-python" || got.Language != "python" || len(got.Entities) != len(want) {
		t.Fatalf("response = %#v, want Python entities %#v", got, want)
	}
	for _, entity := range got.Entities {
		if kind, found := want[entity.QualifiedName]; !found || entity.Kind != kind {
			t.Fatalf("unexpected entity = %#v", entity)
		}
	}
}

func TestRunRejectsInvalidPythonRequestsAndSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.py"), []byte("def run():\n    return None\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(app.py): %v", err)
	}
	valid := `{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"python","files":["app.py"]}`
	for name, input := range map[string]string{
		"wrong language": strings.Replace(valid, `"python"`, `"javascript"`, 1),
		"trailing value": valid + ` {}`,
		"unsafe path":    strings.Replace(valid, `"app.py"`, `"../app.py"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
				t.Fatal("run() error = nil, want request rejection")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "broken.py"), []byte("def broken(:\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(broken.py): %v", err)
	}
	broken := strings.Replace(valid, `"app.py"`, `"broken.py"`, 1)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(broken), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want syntax rejection")
	}
}

func TestRunRejectsSourceSymlinkOutsideRepository(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "outside.py")
	if err := os.WriteFile(external, []byte("def outside():\n    return None\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(external): %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "linked.py")); err != nil {
		t.Fatalf("Symlink(): %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"python","files":["linked.py"]}`)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want source symlink rejection")
	}
}
