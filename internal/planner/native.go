package planner

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/indexing"
	"github.com/zeebo/blake3"
)

type coordinatorIndexer struct {
	coordinator indexing.Coordinator
}

func NewNativeRunner(config NativeConfig) *NativeRunner {
	indexer := config.Indexer
	if indexer == nil {
		indexer = coordinatorIndexer{coordinator: indexing.NewCoordinator()}
	}
	return &NativeRunner{indexer: indexer, tempDir: config.TempDir}
}

func (c coordinatorIndexer) Index(ctx context.Context, root, sourceSHA string) ([]domain.LanguageProjection, error) {
	return c.coordinator.Index(ctx, root, sourceSHA)
}

func (e *NativeRunner) IndexSource(ctx context.Context, request SourceRequest) (SourceEvidence, error) {
	if err := validateSourceRequest(request); err != nil {
		return SourceEvidence{}, err
	}
	workspace, err := os.MkdirTemp(e.tempDir, "thread-keep-runner-")
	if err != nil {
		return SourceEvidence{}, domain.NewError(domain.CodeLocalStorage, errors.New("create runner workspace"))
	}
	defer os.RemoveAll(workspace)
	checkout := filepath.Join(workspace, "checkout")
	if err := e.initializeCheckout(ctx, checkout, request); err != nil {
		return SourceEvidence{}, err
	}
	sourceSHA, previewIdentity, treeDigest, err := e.materialize(ctx, checkout, request)
	if err != nil {
		return SourceEvidence{}, err
	}
	projections, err := e.indexer.Index(ctx, checkout, sourceSHA)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return SourceEvidence{}, domain.NewError(domain.CodeBusy, errors.New("runner indexing timed out or was cancelled"))
		}
		return SourceEvidence{}, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner indexer failed"))
	}
	entities, provenance, err := collectEvidence(projections, sourceSHA)
	if err != nil {
		return SourceEvidence{}, err
	}
	return SourceEvidence{RepositoryID: request.RepositoryID, TargetRef: request.TargetRef, Mode: request.Mode, SourceSHA: sourceSHA, PreviewIdentity: previewIdentity, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, GitTreeDigest: treeDigest, EntityShapeDigest: domain.DigestSourceEvidence(entities), Entities: entities, Provenance: provenance, CoverageComplete: true, WorkerVersion: WorkerVersion}, nil
}

func (e *NativeRunner) initializeCheckout(ctx context.Context, checkout string, request SourceRequest) error {
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		return domain.NewError(domain.CodeLocalStorage, errors.New("create checkout directory"))
	}
	if request.Credential != "" {
		if err := ensureAskPass(checkout); err != nil {
			return domain.NewError(domain.CodeLocalStorage, errors.New("configure one-job checkout authentication"))
		}
	}
	if err := runGit(ctx, checkout, request.Credential, "init", "--quiet"); err != nil {
		return gitFailure(ctx, domain.CodeLocalStorage, "initialize isolated checkout")
	}
	if err := runGit(ctx, checkout, request.Credential, "remote", "add", "origin", request.RepositoryURL); err != nil {
		return gitFailure(ctx, domain.CodeValidation, "configure isolated checkout")
	}
	shas := []string{request.FinalSHA}
	if request.Mode == SourcePreview {
		shas = []string{request.BaseSHA, request.HeadSHA}
	}
	for _, sha := range shas {
		if err := runGit(ctx, checkout, request.Credential, "fetch", "--quiet", "--no-tags", "--depth=128", "origin", sha); err != nil {
			return gitFailure(ctx, domain.CodeEntityNotFound, "fetch requested source revision")
		}
	}
	return nil
}

func (e *NativeRunner) materialize(ctx context.Context, checkout string, request SourceRequest) (string, string, string, error) {
	if request.Mode == SourceFinal {
		if err := runGit(ctx, checkout, request.Credential, "checkout", "--quiet", "--detach", request.FinalSHA); err != nil {
			return "", "", "", gitFailure(ctx, domain.CodeEntityNotFound, "checkout final source revision")
		}
		tree, err := gitOutput(ctx, checkout, request.Credential, "rev-parse", "HEAD^{tree}")
		if err != nil {
			return "", "", "", gitFailure(ctx, domain.CodeLocalStorage, "resolve final source tree")
		}
		return strings.ToLower(request.FinalSHA), "", tree, nil
	}
	if err := runGit(ctx, checkout, request.Credential, "checkout", "--quiet", "--detach", request.BaseSHA); err != nil {
		return "", "", "", gitFailure(ctx, domain.CodeEntityNotFound, "checkout preview base revision")
	}
	if err := runGit(ctx, checkout, request.Credential, "merge", "--quiet", "--no-commit", "--no-ff", request.HeadSHA); err != nil {
		_ = runGit(context.Background(), checkout, "", "merge", "--abort")
		if ctx.Err() != nil {
			return "", "", "", domain.NewError(domain.CodeBusy, errors.New("preview merge timed out or was cancelled"))
		}
		return "", "", "", domain.NewError(domain.CodeRemoteConflict, errors.New("preview source revisions do not merge cleanly"))
	}
	tree, err := gitOutput(ctx, checkout, request.Credential, "write-tree")
	if err != nil {
		return "", "", "", gitFailure(ctx, domain.CodeLocalStorage, "write preview source tree")
	}
	digest := blake3.Sum256([]byte("preview\x00" + strings.ToLower(request.BaseSHA) + "\x00" + strings.ToLower(request.HeadSHA) + "\x00" + tree))
	return strings.ToLower(request.HeadSHA), fmt.Sprintf("%x", digest[:]), tree, nil
}

func collectEvidence(projections []domain.LanguageProjection, sourceSHA string) ([]domain.Entity, []domain.ContextSnapshotProvenance, error) {
	if len(projections) == 0 {
		return nil, nil, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner produced no complete indexer provenance"))
	}
	entities := make([]domain.Entity, 0)
	provenance := make([]domain.ContextSnapshotProvenance, 0, len(projections))
	seenLanguages := make(map[string]bool, len(projections))
	for _, projection := range projections {
		coverage := projection.Coverage
		if coverage.State != domain.CoverageIndexed || coverage.Language == "" || coverage.IndexerID == "" || coverage.IndexerVersion == "" || seenLanguages[coverage.Language] {
			return nil, nil, domain.NewError(domain.CodeCoverageIncomplete, errors.New("planner indexing coverage or provenance is incomplete"))
		}
		seenLanguages[coverage.Language] = true
		provenance = append(provenance, domain.ContextSnapshotProvenance{Language: coverage.Language, IndexerID: coverage.IndexerID, IndexerVersion: coverage.IndexerVersion, SourceSHA: sourceSHA})
		if len(entities)+len(projection.Entities) > maxEvidenceEntities {
			return nil, nil, domain.NewError(domain.CodeCoverageIncomplete, errors.New("planner evidence exceeds the entity limit"))
		}
		for _, entity := range projection.Entities {
			if entity.SourceSHA != sourceSHA || entity.Language != coverage.Language || entity.Key == "" || entity.StructuralHash == "" {
				return nil, nil, domain.NewError(domain.CodeCoverageIncomplete, errors.New("planner indexer returned invalid source evidence"))
			}
			entities = append(entities, entity)
		}
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].Key < entities[j].Key })
	for index := 1; index < len(entities); index++ {
		if entities[index-1].Key == entities[index].Key {
			return nil, nil, domain.NewError(domain.CodeCoverageIncomplete, errors.New("planner evidence contains duplicate entity keys"))
		}
	}
	sort.Slice(provenance, func(i, j int) bool { return provenance[i].Language < provenance[j].Language })
	return entities, provenance, nil
}

func validateSourceRequest(request SourceRequest) error {
	if request.RepositoryID == "" || request.TargetRef == "" || request.RepositoryURL == "" || !validRepositoryURL(request.RepositoryURL) {
		return domain.NewError(domain.CodeValidation, errors.New("planner source request is incomplete"))
	}
	switch request.Mode {
	case SourcePreview:
		if !validSHA(request.BaseSHA) || !validSHA(request.HeadSHA) || request.FinalSHA != "" {
			return domain.NewError(domain.CodeValidation, errors.New("preview source request revisions are invalid"))
		}
	case SourceFinal:
		if !validSHA(request.FinalSHA) || request.BaseSHA != "" || request.HeadSHA != "" {
			return domain.NewError(domain.CodeValidation, errors.New("final source request revision is invalid"))
		}
	default:
		return domain.NewError(domain.CodeValidation, errors.New("planner source mode is invalid"))
	}
	return nil
}

func ValidateSourceRequest(request SourceRequest) error {
	return validateSourceRequest(request)
}

func validRepositoryURL(value string) bool {
	if filepath.IsAbs(value) {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil {
		return false
	}
	return (parsed.Scheme == "https" || parsed.Scheme == "file") && parsed.Host != "" || parsed.Scheme == "file" && parsed.Path != ""
}

func validSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func runGit(ctx context.Context, root, credential string, arguments ...string) error {
	_, err := gitCommand(ctx, root, credential, arguments...).CombinedOutput()
	return err
}

func gitOutput(ctx context.Context, root, credential string, arguments ...string) (string, error) {
	output, err := gitCommand(ctx, root, credential, arguments...).Output()
	return strings.TrimSpace(string(output)), err
}

func gitCommand(ctx context.Context, root, credential string, arguments ...string) *exec.Cmd {
	secured := []string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", "http.followRedirects=false", "-C", root}
	command := exec.CommandContext(ctx, "git", append(secured, arguments...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	if credential != "" {
		command.Env = append(command.Env, "THREAD_KEEP_CHECKOUT_TOKEN="+credential, "GIT_ASKPASS="+askPassPath(root))
	}
	return command
}

func askPassPath(root string) string { return filepath.Join(filepath.Dir(root), "askpass") }

func ensureAskPass(root string) error {
	path := askPassPath(root)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	contents := "#!/bin/sh\ncase \"$1\" in *Username*) printf '%s\\n' x-access-token ;; *) printf '%s\\n' \"$THREAD_KEEP_CHECKOUT_TOKEN\" ;; esac\n"
	return os.WriteFile(path, []byte(contents), 0o700)
}

func gitFailure(ctx context.Context, code domain.ErrorCode, message string) error {
	if ctx.Err() != nil {
		return domain.NewError(domain.CodeBusy, errors.New("planner Git operation timed out or was cancelled"))
	}
	return domain.NewError(code, errors.New(message))
}
