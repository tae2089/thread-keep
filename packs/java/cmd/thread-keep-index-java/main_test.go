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

func TestRunExtractsScopedJavaEntities(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"src/Service.java": `package example;

public class Service {
    private int value = 1;
    private Runnable callback = () -> {};

    Service() {}

    public void run() {}

    static class Nested {
        void call() {}
    }
}

interface Runner {
    void execute();
}

enum Mode {
    FAST {
        void constantHidden() {}
    };
    void apply() {}
}

record User(String name) {
    User {}
    String display() { return name; }
}

@interface Marker {
    String value();
}
`,
		"src/Utility.java": `class Utility {
    static <T> T identity(T value) { return value; }
}
`,
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
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"java","files":["src/Service.java","src/Utility.java"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]string{
		"Marker":              "interface",
		"Marker.value":        "method",
		"Mode":                "enum",
		"Mode.apply":          "method",
		"Runner":              "interface",
		"Runner.execute":      "method",
		"Service":             "class",
		"Service.<init>":      "method",
		"Service.Nested":      "class",
		"Service.Nested.call": "method",
		"Service.run":         "method",
		"User":                "type",
		"User.<init>":         "method",
		"User.display":        "method",
		"Utility":             "class",
		"Utility.identity":    "method",
	}
	if got.Indexer.ID != "thread-keep-index-java" || got.Indexer.Version != "dev" || got.Language != "java" || len(got.Entities) != len(want) {
		t.Fatalf("response = %#v, want Java entities %#v", got, want)
	}
	for _, entity := range got.Entities {
		if kind, found := want[entity.QualifiedName]; !found || entity.Kind != kind {
			t.Fatalf("unexpected entity = %#v", entity)
		}
	}
}

func TestRunRejectsInvalidJavaRequestsAndSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Main.java"), []byte("class Main {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(Main.java): %v", err)
	}
	valid := `{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"java","files":["Main.java"]}`
	for name, input := range map[string]string{
		"wrong language": strings.Replace(valid, `"java"`, `"python"`, 1),
		"trailing value": valid + ` {}`,
		"unsafe path":    strings.Replace(valid, `"Main.java"`, `"../Main.java"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
				t.Fatal("run() error = nil, want request rejection")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "Broken.java"), []byte("class Broken { void run( { }"), 0o644); err != nil {
		t.Fatalf("WriteFile(Broken.java): %v", err)
	}
	broken := strings.Replace(valid, `"Main.java"`, `"Broken.java"`, 1)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(broken), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want syntax rejection")
	}
}

func TestRunOmitsAnonymousClassMembersWithoutStableOwners(t *testing.T) {
	root := t.TempDir()
	contents := `class Service {
    void run() {
        Runnable callback = new Runnable() {
            public void hidden() {}
        };
    }
}
`
	if err := os.WriteFile(filepath.Join(root, "Service.java"), []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(Service.java): %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"java","files":["Service.java"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Entities) != 2 {
		t.Fatalf("entities = %#v, want only Service and Service.run", got.Entities)
	}
	for _, entity := range got.Entities {
		if entity.QualifiedName == "Service.run.hidden" {
			t.Fatalf("anonymous class member leaked as a named entity: %#v", entity)
		}
	}
}

func TestRunOmitsAnonymousClassMembersFromClassInitializers(t *testing.T) {
	root := t.TempDir()
	contents := `class Service {
    Runnable field = new Runnable() {
        public void fieldHidden() {}
    };

    static {
        Runnable local = new Runnable() {
            public void staticHidden() {}
        };
    }
}
`
	if err := os.WriteFile(filepath.Join(root, "Service.java"), []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(Service.java): %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"java","files":["Service.java"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Entities) != 1 || got.Entities[0].QualifiedName != "Service" {
		t.Fatalf("entities = %#v, want only the named Service class", got.Entities)
	}
}

func TestRunRejectsSourceSymlinkOutsideRepository(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "Outside.java")
	if err := os.WriteFile(external, []byte("class Outside {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(external): %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "Linked.java")); err != nil {
		t.Fatalf("Symlink(): %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"java","files":["Linked.java"]}`)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want source symlink rejection")
	}
}
