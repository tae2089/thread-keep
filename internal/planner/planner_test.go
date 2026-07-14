package planner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

type blockingIndexer struct{}

type oversizedIndexer struct{}

func TestNativeRunnerIndexesFinalSHAWithoutRunningRepositoryHooks(t *testing.T) {
	repository, baseSHA, _ := testRepository(t, false)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(repository, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatalf("write source hook: %v", err)
	}
	executor := NewNativeRunner(NativeConfig{})
	evidence, err := executor.IndexSource(context.Background(), SourceRequest{Mode: SourceFinal, RepositoryID: "repo-id", TargetRef: "refs/contexts/main", RepositoryURL: repository, FinalSHA: baseSHA})
	if err != nil {
		t.Fatalf("IndexSource(final) error = %v", err)
	}
	if evidence.SourceSHA != baseSHA || evidence.GitTreeDigest == "" || evidence.EntityShapeDigest == "" || !evidence.CoverageComplete || len(evidence.Provenance) != 1 || len(evidence.Entities) == 0 {
		t.Fatalf("IndexSource(final) = %+v", evidence)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository hook executed, marker stat = %v", err)
	}
}

func TestNativeRunnerReportsPreviewConflictAndFinalMissingSHA(t *testing.T) {
	repository, baseSHA, headSHA := testRepository(t, true)
	executor := NewNativeRunner(NativeConfig{})
	_, err := executor.IndexSource(context.Background(), SourceRequest{Mode: SourcePreview, RepositoryID: "repo-id", TargetRef: "refs/contexts/main", RepositoryURL: repository, BaseSHA: baseSHA, HeadSHA: headSHA})
	if domain.CodeOf(err) != domain.CodeRemoteConflict {
		t.Fatalf("IndexSource(preview conflict) error = %v, want remote conflict", err)
	}
	_, err = executor.IndexSource(context.Background(), SourceRequest{Mode: SourceFinal, RepositoryID: "repo-id", TargetRef: "refs/contexts/main", RepositoryURL: repository, FinalSHA: strings.Repeat("f", 40)})
	if domain.CodeOf(err) != domain.CodeEntityNotFound {
		t.Fatalf("IndexSource(missing final SHA) error = %v, want entity not found", err)
	}
}

func TestNativeRunnerBuildsDeterministicPreviewEvidence(t *testing.T) {
	repository, baseSHA, _ := testRepository(t, false)
	writeFile(t, filepath.Join(repository, "extra.go"), "package example\n\nfunc Extra() string { return \"ok\" }\n")
	testGit(t, repository, "add", "extra.go")
	testGit(t, repository, "commit", "-qm", "feature")
	headSHA := testGit(t, repository, "rev-parse", "HEAD")
	executor := NewNativeRunner(NativeConfig{})
	request := SourceRequest{Mode: SourcePreview, RepositoryID: "repo-id", TargetRef: "refs/contexts/main", RepositoryURL: repository, BaseSHA: baseSHA, HeadSHA: headSHA}
	first, err := executor.IndexSource(context.Background(), request)
	if err != nil {
		t.Fatalf("IndexSource(preview first) error = %v", err)
	}
	second, err := executor.IndexSource(context.Background(), request)
	if err != nil {
		t.Fatalf("IndexSource(preview second) error = %v", err)
	}
	if first.SourceSHA != headSHA || first.PreviewIdentity == "" || first.PreviewIdentity != second.PreviewIdentity || first.GitTreeDigest != second.GitTreeDigest || first.EntityShapeDigest != second.EntityShapeDigest {
		t.Fatalf("preview evidence is not deterministic: first=%+v second=%+v", first, second)
	}
}

func TestNativeRunnerEnforcesTimeoutEntityLimitAndCredentialRedaction(t *testing.T) {
	repository, baseSHA, _ := testRepository(t, false)
	t.Run("timeout", func(t *testing.T) {
		executor := NewNativeRunner(NativeConfig{Indexer: blockingIndexer{}})
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, err := executor.IndexSource(ctx, SourceRequest{Mode: SourceFinal, RepositoryID: "repo-id", TargetRef: "refs/contexts/main", RepositoryURL: repository, FinalSHA: baseSHA})
		if domain.CodeOf(err) != domain.CodeBusy {
			t.Fatalf("IndexSource(timeout) error = %v, want busy", err)
		}
	})
	t.Run("entity limit", func(t *testing.T) {
		executor := NewNativeRunner(NativeConfig{Indexer: oversizedIndexer{}})
		_, err := executor.IndexSource(context.Background(), SourceRequest{Mode: SourceFinal, RepositoryID: "repo-id", TargetRef: "refs/contexts/main", RepositoryURL: repository, FinalSHA: baseSHA})
		if domain.CodeOf(err) != domain.CodeCoverageIncomplete {
			t.Fatalf("IndexSource(oversized) error = %v, want coverage incomplete", err)
		}
	})
	t.Run("credential redaction", func(t *testing.T) {
		const credential = "checkout-credential-fixture"
		executor := NewNativeRunner(NativeConfig{})
		_, err := executor.IndexSource(context.Background(), SourceRequest{Mode: SourceFinal, RepositoryID: "repo-id", TargetRef: "refs/contexts/main", RepositoryURL: "https://127.0.0.1:1/owner/repository.git", Credential: credential, FinalSHA: baseSHA})
		if err == nil || strings.Contains(err.Error(), credential) {
			t.Fatalf("IndexSource(invalid remote) secret-safe error = %v", err)
		}
	})
}

func TestProcessRunnerEnforcesOutputLimit(t *testing.T) {
	script := filepath.Join(t.TempDir(), "oversized-worker")
	contents := "#!/bin/sh\nhead -c 2048 /dev/zero | tr '\\0' x\n"
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake worker: %v", err)
	}
	executor := ProcessRunner{Path: script, MaxOutputBytes: 1024, Timeout: 10 * time.Second}
	_, err := executor.IndexSource(context.Background(), SourceRequest{Mode: SourceFinal, RepositoryID: "repo-id", TargetRef: "refs/contexts/main", RepositoryURL: "/tmp/repository", FinalSHA: strings.Repeat("a", 40)})
	if domain.CodeOf(err) != domain.CodeCoverageIncomplete {
		t.Fatalf("ProcessRunner.IndexSource() error = %v, want coverage incomplete", err)
	}
}

func TestProcessRunnerRoundTripsThroughRunnerCommand(t *testing.T) {
	repository, finalSHA, _ := testRepository(t, false)
	binary := filepath.Join(t.TempDir(), "thread-keep-runner")
	command := exec.Command("go", "build", "-trimpath", "-o", binary, "../../cmd/thread-keep-runner")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build thread-keep-runner: %v: %s", err, output)
	}
	runner := ProcessRunner{Path: binary, Timeout: 30 * time.Second}
	evidence, err := runner.IndexSource(context.Background(), SourceRequest{Mode: SourceFinal, RepositoryID: "repo-id", TargetRef: "refs/contexts/main", RepositoryURL: repository, FinalSHA: finalSHA})
	if err != nil {
		t.Fatalf("ProcessRunner.IndexSource() error = %v", err)
	}
	if evidence.SourceSHA != finalSHA || evidence.WorkerVersion != WorkerVersion || evidence.GitTreeDigest == "" || evidence.EntityShapeDigest == "" || !evidence.CoverageComplete {
		t.Fatalf("ProcessRunner.IndexSource() evidence = %+v", evidence)
	}
}

func (blockingIndexer) Index(ctx context.Context, _ string, _ string) ([]domain.LanguageProjection, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (oversizedIndexer) Index(context.Context, string, string) ([]domain.LanguageProjection, error) {
	entities := make([]domain.Entity, maxEvidenceEntities+1)
	return []domain.LanguageProjection{{Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1"}, Entities: entities}}, nil
}

func testRepository(t *testing.T, conflicting bool) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	testGit(t, root, "init", "-q")
	testGit(t, root, "config", "user.name", "Thread Keep Test")
	testGit(t, root, "config", "user.email", "thread-keep@example.invalid")
	writeFile(t, filepath.Join(root, "main.go"), "package example\n\nfunc Value() int { return 0 }\n")
	testGit(t, root, "add", "main.go")
	testGit(t, root, "commit", "-qm", "root")
	rootSHA := testGit(t, root, "rev-parse", "HEAD")
	if !conflicting {
		return root, rootSHA, ""
	}
	testGit(t, root, "checkout", "-qb", "target")
	writeFile(t, filepath.Join(root, "main.go"), "package example\n\nfunc Value() int { return 1 }\n")
	testGit(t, root, "commit", "-qam", "target")
	baseSHA := testGit(t, root, "rev-parse", "HEAD")
	testGit(t, root, "checkout", "-qb", "feature", rootSHA)
	writeFile(t, filepath.Join(root, "main.go"), "package example\n\nfunc Value() int { return 2 }\n")
	testGit(t, root, "commit", "-qam", "feature")
	return root, baseSHA, testGit(t, root, "rev-parse", "HEAD")
}

func testGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestDigestSourceEvidenceIgnoresSourceSHA(t *testing.T) {
	left := []domain.Entity{{Language: "go", Key: "example.Value", Kind: domain.EntityFunction, Name: "Value", Path: "main.go", StartLine: 3, EndLine: 3, SourceSHA: strings.Repeat("a", 40), StructuralHash: strings.Repeat("b", 64)}}
	right := append([]domain.Entity(nil), left...)
	right[0].SourceSHA = strings.Repeat("c", 40)
	if domain.DigestSourceEvidence(left) != domain.DigestSourceEvidence(right) {
		t.Fatal("DigestSourceEvidence() changed for source SHA only")
	}
}

func ExampleSourceRequest() {
	fmt.Println(SourceFinal)
	// Output: final
}
