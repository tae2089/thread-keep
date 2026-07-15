package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
)

var _ SourceRunner = ProcessRunner{}
var _ SourceRunner = (*NativeRunner)(nil)

func TestSourceRunnerRequestJSONContractRemainsStable(t *testing.T) {
	request := SourceRequest{
		Mode:          SourcePreview,
		RepositoryID:  "repository-id",
		TargetRef:     "refs/contexts/main",
		RepositoryURL: "/repository",
		Credential:    "redacted-fixture",
		BaseSHA:       strings.Repeat("a", 40),
		HeadSHA:       strings.Repeat("b", 40),
		FinalSHA:      strings.Repeat("c", 40),
	}
	contents, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("json.Marshal(SourceRequest) error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(contents, &fields); err != nil {
		t.Fatalf("json.Unmarshal(SourceRequest) error = %v", err)
	}
	want := []string{"base_sha", "credential", "final_sha", "head_sha", "mode", "repository_id", "repository_url", "target_ref"}
	for _, field := range want {
		if _, ok := fields[field]; !ok {
			t.Errorf("SourceRequest JSON missing field %q", field)
		}
	}
	if len(fields) != len(want) {
		t.Fatalf("SourceRequest JSON fields = %v, want exactly %v", fields, want)
	}
}

func TestWorkerResponseJSONContractRemainsStable(t *testing.T) {
	request := SourceRequest{
		Mode:          SourceFinal,
		RepositoryID:  "repository-id",
		TargetRef:     "refs/contexts/main",
		RepositoryURL: "/missing-repository",
		FinalSHA:      strings.Repeat("a", 40),
	}
	input, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("json.Marshal(SourceRequest) error = %v", err)
	}
	var output bytes.Buffer
	if err := RunWorker(context.Background(), bytes.NewReader(input), &output); err != nil {
		t.Fatalf("RunWorker() error = %v", err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("json.Unmarshal(worker response) error = %v", err)
	}
	for _, field := range []string{"evidence", "code", "message"} {
		if _, ok := response[field]; !ok {
			t.Errorf("worker response JSON missing field %q", field)
		}
	}
	if len(response) != 3 {
		t.Fatalf("worker response JSON fields = %v, want evidence, code, and message only", response)
	}
	var code domain.ErrorCode
	if err := json.Unmarshal(response["code"], &code); err != nil || code == "" {
		t.Fatalf("worker response code = %q, error = %v", code, err)
	}
}

func TestValidateSourceEvidenceRejectsMismatchedWorkerVersion(t *testing.T) {
	sourceSHA := strings.Repeat("a", 40)
	request := SourceRequest{Mode: SourceFinal, RepositoryID: "repository-id", TargetRef: "refs/contexts/main", FinalSHA: sourceSHA}
	entity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: sourceSHA, StructuralHash: "hash"}
	evidence := SourceEvidence{
		RepositoryID:      request.RepositoryID,
		TargetRef:         request.TargetRef,
		Mode:              request.Mode,
		SourceSHA:         sourceSHA,
		GitTreeDigest:     "tree-digest",
		EntityShapeDigest: domain.DigestSourceEvidence([]domain.Entity{entity}),
		Entities:          []domain.Entity{entity},
		Provenance:        []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: sourceSHA}},
		CoverageComplete:  true,
		WorkerVersion:     WorkerVersion + "-mismatch",
	}

	if err := ValidateSourceEvidence(request, evidence); domain.CodeOf(err) != domain.CodeCoverageIncomplete {
		t.Fatalf("ValidateSourceEvidence() error = %v, want coverage incomplete", err)
	}
}
