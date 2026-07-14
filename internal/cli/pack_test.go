package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/tae2089/thread-keep/internal/app"
)

func TestPackInstallUsesCurrentPyPIInterpreterWithoutOpeningRepository(t *testing.T) {
	python := filepath.Join(t.TempDir(), "python")
	if err := os.WriteFile(python, []byte("python"), 0o755); err != nil {
		t.Fatalf("WriteFile(python): %v", err)
	}
	t.Setenv("THREAD_KEEP_PYTHON_EXECUTABLE", python)
	t.Setenv("THREAD_KEEP_PACKAGE_VERSION", "1.2.3")
	opened := false
	var got []string
	runner := NewRunner(func(context.Context, string) (*app.Service, error) {
		opened = true
		return nil, nil
	})
	runner.runProcess = func(_ context.Context, command []string, _ io.Reader, _, _ io.Writer) error {
		got = append([]string(nil), command...)
		return nil
	}
	root := packTestRoot(runner)
	var stdout, stderr bytes.Buffer

	code := runner.Execute(context.Background(), root, []string{"pack", "install", "python", "typescript", "python"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("pack install exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	want := []string{python, "-m", "pip", "install", "thread-keep[typescript,python]==1.2.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("process command = %v, want %v", got, want)
	}
	if opened {
		t.Fatal("pack install opened a repository service")
	}
	if !strings.Contains(stdout.String(), "typescript,python") {
		t.Fatalf("stdout = %q, want installed languages", stdout.String())
	}
}

func TestPackInstallRequiresPyPIEnvironmentAndKnownLanguages(t *testing.T) {
	tests := map[string]struct {
		arguments []string
		python    string
		version   string
	}{
		"missing language":         {arguments: []string{"pack", "install"}},
		"unknown language":         {arguments: []string{"pack", "install", "ruby"}, python: "/tmp/python", version: "1.2.3"},
		"missing PyPI environment": {arguments: []string{"pack", "install", "typescript"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("THREAD_KEEP_PYTHON_EXECUTABLE", test.python)
			t.Setenv("THREAD_KEEP_PACKAGE_VERSION", test.version)
			runner := NewRunner(nil)
			runner.runProcess = func(context.Context, []string, io.Reader, io.Writer, io.Writer) error {
				t.Fatal("process runner called for invalid input")
				return nil
			}
			var stdout, stderr bytes.Buffer
			code := runner.Execute(context.Background(), packTestRoot(runner), test.arguments, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), "validation") {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestPackInstallJSONSuppressesPipOutput(t *testing.T) {
	python := filepath.Join(t.TempDir(), "python")
	if err := os.WriteFile(python, []byte("python"), 0o755); err != nil {
		t.Fatalf("WriteFile(python): %v", err)
	}
	t.Setenv("THREAD_KEEP_PYTHON_EXECUTABLE", python)
	t.Setenv("THREAD_KEEP_PACKAGE_VERSION", "1.2.3")
	runner := NewRunner(nil)
	runner.runProcess = func(_ context.Context, _ []string, _ io.Reader, stdout, stderr io.Writer) error {
		_, _ = io.WriteString(stdout, "pip stdout\n")
		_, _ = io.WriteString(stderr, "pip stderr\n")
		return nil
	}
	var stdout, stderr bytes.Buffer

	code := runner.Execute(context.Background(), packTestRoot(runner), []string{"--json", "pack", "install", "rust"}, &stdout, &stderr)

	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var result struct {
		Version int               `json:"version"`
		Data    PackInstallResult `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Version != OutputVersion || !reflect.DeepEqual(result.Data.Languages, []string{"rust"}) {
		t.Fatalf("JSON result = %#v, decode error = %v, raw = %q", result, err, stdout.String())
	}
}

func packTestRoot(runner *Runner) *cobra.Command {
	root := &cobra.Command{Use: "thread-keep", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().String("repo", "", "")
	root.AddCommand(Commands(runner)...)
	return root
}
