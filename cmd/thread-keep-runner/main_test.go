package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
)

type timeoutObservingRunner struct {
	cancelled chan error
}

func (r timeoutObservingRunner) IndexSource(ctx context.Context, _ planner.SourceRequest) (planner.SourceEvidence, error) {
	<-ctx.Done()
	r.cancelled <- ctx.Err()
	return planner.SourceEvidence{}, domain.NewError(domain.CodeBusy, errors.New("execution timed out"))
}

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

func TestRunnerCommandAppliesExecutionTimeoutToSourceRunner(t *testing.T) {
	root := t.TempDir()
	request := filepath.Join(root, "request.json")
	credential := filepath.Join(root, "credential")
	result := filepath.Join(root, "result.json")
	requestPayload := `{"mode":"final","repository_id":"repository","target_ref":"refs/contexts/main","repository_url":"https://github.com/owner/repository.git","final_sha":"` + strings.Repeat("a", 40) + `"}`
	if err := os.WriteFile(request, []byte(requestPayload), 0o600); err != nil {
		t.Fatalf("write request error = %v", err)
	}
	if err := os.WriteFile(credential, []byte("token"), 0o400); err != nil {
		t.Fatalf("write credential error = %v", err)
	}
	cancelled := make(chan error, 1)
	runner := timeoutObservingRunner{cancelled: cancelled}
	var stderr bytes.Buffer
	exitCode := runWithRunner([]string{"execute", "--request-file=" + request, "--credential-file=" + credential, "--result-file=" + result, "--credential-wait-timeout=1s", "--execution-timeout=10ms"}, strings.NewReader(""), io.Discard, &stderr, runner)
	if exitCode != 0 {
		t.Fatalf("runWithRunner() exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("source runner cancellation = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source runner did not receive execution timeout")
	}
}
