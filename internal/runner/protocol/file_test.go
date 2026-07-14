package protocol

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
)

type fileTestRunner struct{}

func (fileTestRunner) IndexSource(_ context.Context, request planner.SourceRequest) (planner.SourceEvidence, error) {
	return planner.SourceEvidence{RepositoryID: request.RepositoryID, TargetRef: request.TargetRef, Mode: request.Mode, SourceSHA: request.FinalSHA, GitTreeDigest: strings.Repeat("c", 64), EntityShapeDigest: domain.DigestSourceEvidence(nil), Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "go", IndexerVersion: "1", SourceSHA: request.FinalSHA}}, CoverageComplete: true, WorkerVersion: planner.WorkerVersion}, nil
}

func TestExecuteFilesKeepsCredentialOutOfRequestAndPublishesAtomicResult(t *testing.T) {
	root := t.TempDir()
	requestPath := filepath.Join(root, "request.json")
	credentialPath := filepath.Join(root, "credential")
	resultPath := filepath.Join(root, "result.json")
	request := `{"mode":"final","repository_id":"repository","target_ref":"refs/contexts/main","repository_url":"https://github.com/owner/repository.git","final_sha":"` + strings.Repeat("b", 40) + `"}`
	if err := os.WriteFile(requestPath, []byte(request), 0o600); err != nil {
		t.Fatalf("write request error = %v", err)
	}
	if err := os.WriteFile(credentialPath, []byte("one-job-secret\n"), 0o400); err != nil {
		t.Fatalf("write credential error = %v", err)
	}
	if err := ExecuteFiles(context.Background(), FileExecutionOptions{RequestPath: requestPath, CredentialPath: credentialPath, ResultPath: resultPath}, fileTestRunner{}); err != nil {
		t.Fatalf("ExecuteFiles() error = %v", err)
	}
	contents, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("ReadFile(result) error = %v", err)
	}
	if strings.Contains(string(contents), "one-job-secret") {
		t.Fatal("result contains credential")
	}
	envelope, err := DecodeResult(contents)
	if err != nil || envelope.Evidence.SourceSHA != strings.Repeat("b", 40) {
		t.Fatalf("DecodeResult() = %+v, %v", envelope, err)
	}
}
