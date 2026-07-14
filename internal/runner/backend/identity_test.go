package backend

import (
	"strings"
	"testing"

	"github.com/tae2089/thread-keep/internal/planner"
)

func TestRequestDigestExcludesCredentialAndNormalizesSHA(t *testing.T) {
	request := planner.SourceRequest{Mode: planner.SourceFinal, RepositoryID: "repository", TargetRef: "refs/contexts/main", RepositoryURL: "https://github.com/owner/repository.git", Credential: "first-secret", FinalSHA: strings.Repeat("A", 40)}
	first, err := RequestDigest(request)
	if err != nil {
		t.Fatalf("RequestDigest(first) error = %v", err)
	}
	request.Credential = "second-secret"
	request.FinalSHA = strings.ToLower(request.FinalSHA)
	second, err := RequestDigest(request)
	if err != nil {
		t.Fatalf("RequestDigest(second) error = %v", err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("RequestDigest() = %q, %q", first, second)
	}
	request.TargetRef = "refs/contexts/release"
	third, err := RequestDigest(request)
	if err != nil {
		t.Fatalf("RequestDigest(changed) error = %v", err)
	}
	if third == first {
		t.Fatal("RequestDigest() ignored non-secret identity change")
	}
}

func TestExecutionAndAttemptIdentityAreStableAndSeparated(t *testing.T) {
	requestDigest := strings.Repeat("a", 64)
	executionID, err := ExecutionID(strings.Repeat("b", 64), requestDigest)
	if err != nil {
		t.Fatalf("ExecutionID() error = %v", err)
	}
	first, err := AttemptID(executionID, 1)
	if err != nil {
		t.Fatalf("AttemptID(1) error = %v", err)
	}
	second, err := AttemptID(executionID, 2)
	if err != nil {
		t.Fatalf("AttemptID(2) error = %v", err)
	}
	if first == second || executionID == first {
		t.Fatalf("identities collided: execution=%s first=%s second=%s", executionID, first, second)
	}
}
