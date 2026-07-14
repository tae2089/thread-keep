package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerCommandRequiresWorkerSubcommand(t *testing.T) {
	var stderr bytes.Buffer
	exitCode := run(nil, strings.NewReader(""), io.Discard, &stderr)
	if exitCode != 2 {
		t.Fatalf("run() exit code = %d, want 2", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "usage: thread-keep-runner worker") {
		t.Fatalf("run() stderr = %q, want canonical runner usage", got)
	}
}

func TestRunnerCommandAcceptsExecuteFileTransport(t *testing.T) {
	root := t.TempDir()
	request := filepath.Join(root, "request.json")
	credential := filepath.Join(root, "credential")
	result := filepath.Join(root, "result.json")
	if err := os.WriteFile(request, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write request error = %v", err)
	}
	if err := os.WriteFile(credential, []byte("token"), 0o400); err != nil {
		t.Fatalf("write credential error = %v", err)
	}
	var stderr bytes.Buffer
	exitCode := run([]string{"execute", "--request-file=" + request, "--credential-file=" + credential, "--result-file=" + result, "--credential-wait-timeout=1s"}, strings.NewReader(""), io.Discard, &stderr)
	if exitCode == 2 {
		t.Fatalf("run(execute) rejected execute subcommand: %s", stderr.String())
	}
}
