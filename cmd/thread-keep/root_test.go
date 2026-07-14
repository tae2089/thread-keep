package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/tae2089/thread-keep/internal/app"
	"github.com/tae2089/thread-keep/internal/cli"
)

func TestRootRegistersExpectedCommands(t *testing.T) {
	root := NewRoot(cli.NewRunner(app.Open))
	var names []string
	for _, command := range root.Commands() {
		names = append(names, command.Name())
	}
	sort.Strings(names)
	want := []string{"candidate", "commit", "context", "diff", "indexers", "init", "landing", "log", "note", "rebuild", "remote", "search", "status", "update"}
	if len(names) != len(want) {
		t.Fatalf("command count = %d, want %d: %v", len(names), len(want), names)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("commands = %v, want %v", names, want)
		}
	}
	if root.PersistentFlags().Lookup("repo") == nil || root.PersistentFlags().Lookup("json") == nil {
		t.Fatal("root must register repository and JSON flags")
	}
}

func TestRootRemainsCompositionOnly(t *testing.T) {
	contents, err := os.ReadFile("root.go")
	if err != nil {
		t.Fatalf("read root.go: %v", err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "root.go", contents, 0)
	if err != nil {
		t.Fatalf("parse root.go: %v", err)
	}
	allowedImports := map[string]bool{
		"github.com/spf13/cobra":                      true,
		"github.com/tae2089/thread-keep/internal/cli": true,
	}
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, `\"`)
		if !allowedImports[path] {
			t.Fatalf("root.go imports %q; composition root must not own lifecycle or output dependencies", path)
		}
	}
	forbidden := map[string]bool{"Execute": true, "Open": true, "Close": true, "writeResult": true, "writeError": true, "exitCode": true}
	ast.Inspect(file, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok && forbidden[identifier.Name] {
			t.Fatalf("root.go references %q; lifecycle/output policy belongs in internal/cli", identifier.Name)
		}
		return true
	})
}
