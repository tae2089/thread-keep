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

func TestRunExtractsScopedKotlinEntities(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"src/App.kt": `package example

class Service(val name: String) {
    constructor(id: Int) : this(id.toString())

    fun run() {}

    fun outer() {
        fun nested() {}
    }

    class Nested {
        fun call() {}
    }

    companion object {
        fun create() = Service("created")
    }
}

interface Runner {
    fun execute()
}

enum class Mode {
    FAST {
        fun hidden() {}
    };
    fun apply() {}
}

object Singleton {
    fun start() {}
}

typealias UserId = String

class WithAnonymous {
    val callback = object : Runnable {
        override fun hidden() {}
    }
}
`,
		"scripts/build.kts": `fun buildProject() {}
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
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"kotlin","files":["scripts/build.kts","src/App.kt"]}`)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	var got response
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := map[string]string{
		"Mode":                     "enum",
		"Mode.apply":               "method",
		"Runner":                   "interface",
		"Runner.execute":           "method",
		"Service":                  "class",
		"Service.<init>":           "method",
		"Service.Companion":        "class",
		"Service.Companion.create": "method",
		"Service.Nested":           "class",
		"Service.Nested.call":      "method",
		"Service.outer":            "method",
		"Service.outer.nested":     "function",
		"Service.run":              "method",
		"Singleton":                "class",
		"Singleton.start":          "method",
		"UserId":                   "type",
		"WithAnonymous":            "class",
		"buildProject":             "function",
	}
	if got.Indexer.ID != "thread-keep-index-kotlin" || got.Indexer.Version != "dev" || got.Language != "kotlin" || len(got.Entities) != len(want) {
		t.Fatalf("response = %#v, want Kotlin entities %#v", got, want)
	}
	for _, entity := range got.Entities {
		if kind, found := want[entity.QualifiedName]; !found || entity.Kind != kind {
			t.Fatalf("unexpected entity = %#v", entity)
		}
	}
}

func TestRunRejectsInvalidKotlinRequestsAndSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "App.kt"), []byte("class App\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(App.kt): %v", err)
	}
	valid := `{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"kotlin","files":["App.kt"]}`
	for name, input := range map[string]string{
		"wrong language": strings.Replace(valid, `"kotlin"`, `"java"`, 1),
		"trailing value": valid + ` {}`,
		"unsafe path":    strings.Replace(valid, `"App.kt"`, `"../App.kt"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(input), &bytes.Buffer{}); err == nil {
				t.Fatal("run() error = nil, want request rejection")
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "Broken.kt"), []byte("class Broken { fun run( { }"), 0o644); err != nil {
		t.Fatalf("WriteFile(Broken.kt): %v", err)
	}
	broken := strings.Replace(valid, `"App.kt"`, `"Broken.kt"`, 1)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, strings.NewReader(broken), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want syntax rejection")
	}
}

func TestRunRejectsSourceSymlinkOutsideRepository(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "Outside.kt")
	if err := os.WriteFile(external, []byte("class Outside\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(external): %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "Linked.kt")); err != nil {
		t.Fatalf("Symlink(): %v", err)
	}
	requestBytes := []byte(`{"protocol_version":1,"repository_root":"` + root + `","source_sha":"sha","language":"kotlin","files":["Linked.kt"]}`)
	if err := run(context.Background(), []string{"index", "--protocol-version=1"}, bytes.NewReader(requestBytes), &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want source symlink rejection")
	}
}
