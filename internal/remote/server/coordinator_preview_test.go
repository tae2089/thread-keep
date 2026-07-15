package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/forge"
	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/tae2089/thread-keep/internal/remote"
	"github.com/zeebo/blake3"
)

type previewFakeForge struct {
	mu             sync.Mutex
	change         forge.Change
	checks         []forge.CheckState
	checkFailures  int
	checkError     error
	getChangeCalls int
}

type previewFakeRunner struct {
	evidence planner.SourceEvidence
}

func TestCoordinatorPreviewIsIdempotentAndDoesNotAdvanceCanonicalRef(t *testing.T) {
	coordinator, store, fakeForge, canonicalRef := newTestCoordinator(t)
	headers := http.Header{"X-Github-Delivery": {"delivery-1"}}
	first, err := coordinator.IntakeWebhook(context.Background(), headers, []byte(`{"event":"fixture"}`))
	if err != nil || !first.Accepted || first.Duplicate || first.Generation != 0 {
		t.Fatalf("IntakeWebhook(first) = %+v, %v", first, err)
	}
	second, err := coordinator.IntakeWebhook(context.Background(), headers, []byte(`{"event":"fixture"}`))
	if err != nil || !second.Duplicate {
		t.Fatalf("IntakeWebhook(duplicate) = %+v, %v", second, err)
	}
	processed, err := coordinator.RunOne(context.Background(), "worker-1", time.Now().UTC())
	if err != nil || !processed {
		t.Fatalf("RunOne(process webhook) = %t, %v", processed, err)
	}
	processed, err = coordinator.RunOne(context.Background(), "worker-1", time.Now().UTC())
	if err != nil || !processed {
		t.Fatalf("RunOne(planning check) = %t, %v", processed, err)
	}
	processed, err = coordinator.RunOne(context.Background(), "worker-1", time.Now().UTC())
	if err != nil || !processed {
		t.Fatalf("RunOne(preview) = %t, %v", processed, err)
	}
	processed, err = coordinator.RunOne(context.Background(), "worker-1", time.Now().UTC())
	if err != nil || !processed {
		t.Fatalf("RunOne(result check) = %t, %v", processed, err)
	}
	plan, err := coordinator.PlanForChange(context.Background(), "repo", fakeForge.change.Key)
	if err != nil || plan.Kind != domain.ContextPlanPreview || plan.Outcome != domain.ContextPlanReady {
		t.Fatalf("PlanForChange() = %+v, %v", plan, err)
	}
	ref, err := store.ReadRef(context.Background(), "context-repo", "refs/contexts/main")
	if err != nil || ref != canonicalRef {
		t.Fatalf("canonical ref after preview = %+v, %v, want %+v", ref, err, canonicalRef)
	}
	if got := fakeForge.checkStates(); len(got) != 2 || got[0] != forge.CheckPlanning || got[1] != forge.CheckReady {
		t.Fatalf("check states = %+v, want planning then ready", got)
	}
	logicalKey := DesiredCheckLogicalKey(fakeForge.change.Key, fakeForge.change.HeadSHA)
	desiredBeforeDuplicate, err := coordinator.refs.DesiredCheck(context.Background(), logicalKey)
	if err != nil || desiredBeforeDuplicate.State != forge.CheckReady {
		t.Fatalf("DesiredCheck(before distinct delivery) = %+v, %v", desiredBeforeDuplicate, err)
	}
	third, err := coordinator.IntakeWebhook(context.Background(), http.Header{"X-Github-Delivery": {"delivery-2"}}, []byte(`{"event":"fixture-again"}`))
	if err != nil || !third.Accepted || third.Duplicate {
		t.Fatalf("IntakeWebhook(distinct duplicate) = %+v, %v", third, err)
	}
	if processed, err := coordinator.RunOne(context.Background(), "worker-duplicate", time.Now().UTC()); err != nil || !processed {
		t.Fatalf("RunOne(distinct duplicate webhook) = %t, %v", processed, err)
	}
	desiredAfterDuplicate, err := coordinator.refs.DesiredCheck(context.Background(), logicalKey)
	if err != nil || desiredAfterDuplicate != desiredBeforeDuplicate {
		t.Fatalf("DesiredCheck(after distinct delivery) = %+v, %v, want unchanged %+v", desiredAfterDuplicate, err, desiredBeforeDuplicate)
	}
	if processed, err := coordinator.RunOne(context.Background(), "worker-duplicate", time.Now().UTC()); err != nil || processed {
		t.Fatalf("RunOne(after distinct duplicate) = %t, %v, want no scheduled work", processed, err)
	}
	if got := fakeForge.checkStates(); len(got) != 2 {
		t.Fatalf("check states after distinct duplicate = %+v, want no new publication", got)
	}
}

func TestCoordinatorCandidatePublicationSupersedesGeneration(t *testing.T) {
	coordinator, _, fakeForge, canonicalRef := newTestCoordinator(t)
	if _, err := coordinator.IntakeWebhook(context.Background(), http.Header{"X-Github-Delivery": {"delivery-1"}}, []byte(`{"event":"fixture"}`)); err != nil {
		t.Fatalf("IntakeWebhook() error = %v", err)
	}
	if processed, err := coordinator.RunOne(context.Background(), "control-candidate", time.Now().UTC()); err != nil || !processed {
		t.Fatalf("RunOne(process webhook) = %t, %v", processed, err)
	}
	delta := domain.CandidateContextDelta{SchemaVersion: 2, Change: fakeForge.change.Key, BaseSourceSHA: fakeForge.change.BaseSHA, HeadSourceSHA: fakeForge.change.HeadSHA, BaseContextCommitID: canonicalRef.CommitID}
	delta, err := domain.NormalizeCandidateContextDelta(delta)
	if err != nil {
		t.Fatalf("NormalizeCandidateContextDelta() error = %v", err)
	}
	digest, _ := domain.CandidateContextDigest(delta)
	result, err := coordinator.PublishCandidate(context.Background(), "repo", remote.CandidatePublicationRequest{Delta: delta, Digest: digest})
	if err != nil || !result.Published || result.Digest != digest {
		t.Fatalf("PublishCandidate(first) = %+v, %v", result, err)
	}
	result, err = coordinator.PublishCandidate(context.Background(), "repo", remote.CandidatePublicationRequest{Delta: delta, Digest: digest})
	if err != nil || result.Published {
		t.Fatalf("PublishCandidate(duplicate) = %+v, %v", result, err)
	}
	generation, err := coordinator.refs.Generation(context.Background(), "repo", fakeForge.change.Key)
	if err != nil || generation.Version != 2 || generation.CandidateDigest != digest || generation.CurrentPlanID != "" {
		t.Fatalf("Generation(after candidate) = %+v, %v", generation, err)
	}
	metadata, err := coordinator.CandidateMetadata(context.Background(), "repo", fakeForge.change.Key.Number)
	if err != nil || metadata.BaseContextCommitID != canonicalRef.CommitID || metadata.HeadSourceSHA != fakeForge.change.HeadSHA {
		t.Fatalf("CandidateMetadata() = %+v, %v", metadata, err)
	}
}

func TestCoordinatorRepairsCheckPublicationAfterRetryExhaustion(t *testing.T) {
	coordinator, _, fakeForge, _ := newTestCoordinator(t)
	fakeForge.mu.Lock()
	fakeForge.checkFailures = 5
	fakeForge.checkError = domain.NewError(domain.CodeLocalStorage, context.DeadlineExceeded)
	fakeForge.mu.Unlock()
	if _, err := coordinator.IntakeWebhook(context.Background(), http.Header{"X-Github-Delivery": {"delivery-check-repair"}}, []byte(`{"event":"fixture"}`)); err != nil {
		t.Fatalf("IntakeWebhook() error = %v", err)
	}
	base := time.Now().UTC()
	if processed, err := coordinator.RunOne(context.Background(), "repair-control", base); err != nil || !processed {
		t.Fatalf("RunOne(process webhook) = %t, %v", processed, err)
	}
	for attempt := 0; attempt < 5; attempt++ {
		processed, err := coordinator.RunOne(context.Background(), "repair-check", base.Add(time.Duration(attempt+1)*time.Minute))
		if !processed || domain.CodeOf(err) != domain.CodeLocalStorage {
			t.Fatalf("RunOne(check failure %d) = %t, %v, want local storage", attempt+1, processed, err)
		}
	}
	if processed, err := coordinator.RunOne(context.Background(), "repair-check", base.Add(6*time.Minute)); err != nil || !processed {
		t.Fatalf("RunOne(repaired check) = %t, %v", processed, err)
	}
	logicalKey := DesiredCheckLogicalKey(fakeForge.change.Key, fakeForge.change.HeadSHA)
	desired, err := coordinator.refs.DesiredCheck(context.Background(), logicalKey)
	if err != nil || desired.PublishedVersion != desired.Version || desired.ProviderCheckRunID == 0 {
		t.Fatalf("DesiredCheck(after repair) = %+v, %v", desired, err)
	}
	if got := fakeForge.checkStates(); len(got) != 6 {
		t.Fatalf("check publication attempts = %d, want 6", len(got))
	}
}

func TestCoordinatorHTTPRoutesExposeCapabilitiesAndCurrentPlan(t *testing.T) {
	coordinator, store, _, _ := newTestCoordinator(t)
	githubAPI := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer "+readerToken {
			_, _ = writer.Write([]byte(`{"permissions":{"push":false,"pull":true}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"permissions":{"push":true,"pull":true}}`))
	}))
	t.Cleanup(githubAPI.Close)
	handler, err := NewCoordinatorHandler(store, coordinator, Config{GitHubAPIBaseURL: githubAPI.URL, Repositories: map[string]RepositoryConfig{
		"repo":  {GitHubOwner: "owner", GitHubRepo: "repository"},
		"other": {GitHubOwner: "owner", GitHubRepo: "other"},
	}})
	if err != nil {
		t.Fatalf("NewCoordinatorHandler() error = %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	if status, payload := call(t, http.MethodGet, server.URL+"/v1/repositories/repo/capabilities", readerToken, nil); status != http.StatusOK || !bytes.Contains(payload, []byte(`"context_planning"`)) {
		t.Fatalf("GET capabilities = %d %s", status, payload)
	}
	webhookHandler, err := NewWebhookHandler(coordinator.ingress)
	if err != nil {
		t.Fatalf("NewWebhookHandler() error = %v", err)
	}
	webhookServer := httptest.NewServer(webhookHandler)
	t.Cleanup(webhookServer.Close)
	request, _ := http.NewRequest(http.MethodPost, webhookServer.URL+"/v1/providers/github/webhooks", strings.NewReader(`{"event":"fixture"}`))
	request.Header.Set("X-Github-Delivery", "delivery-http")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST webhook error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("POST webhook status = %d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodPost, server.URL+"/v1/providers/github/webhooks", strings.NewReader(`{"event":"fixture"}`))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST webhook to context server error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("POST webhook to context server status = %d, want 404", response.StatusCode)
	}
	if processed, err := coordinator.RunOne(context.Background(), "worker-http", time.Now().UTC()); err != nil || !processed {
		t.Fatalf("RunOne(process webhook) = %t, %v", processed, err)
	}
	if processed, err := coordinator.RunOne(context.Background(), "worker-http", time.Now().UTC()); err != nil || !processed {
		t.Fatalf("RunOne(planning check) = %t, %v", processed, err)
	}
	if processed, err := coordinator.RunOne(context.Background(), "worker-http", time.Now().UTC()); err != nil || !processed {
		t.Fatalf("RunOne(preview) = %t, %v", processed, err)
	}
	status, payload := call(t, http.MethodGet, server.URL+"/v1/repositories/repo/pull-requests/42/plan", readerToken, nil)
	if status != http.StatusOK || !bytes.Contains(payload, []byte(`"kind":"preview"`)) {
		t.Fatalf("GET current plan = %d %s", status, payload)
	}
	var plan domain.ContextPlan
	if err := json.Unmarshal(payload, &plan); err != nil {
		t.Fatalf("decode current plan error = %v", err)
	}
	if status, _ := call(t, http.MethodGet, server.URL+"/v1/repositories/other/plans/"+plan.ID, readerToken, nil); status != http.StatusNotFound {
		t.Fatalf("GET plan through another repository = %d, want 404", status)
	}
	if status, _ := call(t, http.MethodGet, server.URL+"/v1/repositories/repo/landings/missing/recovery", readerToken, nil); status != http.StatusForbidden {
		t.Fatalf("GET landing recovery with pull-only token = %d, want 403", status)
	}
	disabled, err := NewHandler(store, Config{GitHubAPIBaseURL: githubAPI.URL, Repositories: map[string]RepositoryConfig{"repo": {GitHubOwner: "owner", GitHubRepo: "repository"}}})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	disabledServer := httptest.NewServer(disabled)
	t.Cleanup(disabledServer.Close)
	if status, _ := call(t, http.MethodGet, disabledServer.URL+"/v1/repositories/repo/capabilities", readerToken, nil); status != http.StatusNotFound {
		t.Fatalf("disabled GET capabilities = %d, want 404", status)
	}
}

func (f *previewFakeForge) DecodeWebhook(headers http.Header, _ []byte) (forge.ForgeEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return forge.ForgeEvent{Provider: "github", DeliveryID: headers.Get("X-GitHub-Delivery"), Action: forge.ForgeActionSynchronize, Change: f.change.Key, BaseRef: f.change.BaseRef, BaseSHA: f.change.BaseSHA, HeadRef: f.change.HeadRef, HeadSHA: f.change.HeadSHA, InstallationID: 7}, nil
}

func (f *previewFakeForge) GetChange(context.Context, domain.ChangeKey) (forge.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getChangeCalls++
	return f.change, nil
}

func (f *previewFakeForge) UpsertCheck(_ context.Context, input forge.CheckInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, input.State)
	return nil
}

func (f *previewFakeForge) ReconcileCheck(_ context.Context, input forge.CheckInput) (forge.CheckPublication, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, input.State)
	if f.checkFailures > 0 {
		f.checkFailures--
		return forge.CheckPublication{}, f.checkError
	}
	checkRunID := input.CheckRunID
	if checkRunID == 0 {
		checkRunID = 1
	}
	return forge.CheckPublication{CheckRunID: checkRunID}, nil
}

func (f *previewFakeForge) CheckoutGrant(context.Context, forge.CheckoutGrantInput) (forge.CheckoutGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return forge.CheckoutGrant{Repository: f.change.Key.Repository, CloneURL: "/tmp/repository", Token: "one-job-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f *previewFakeForge) checkStates() []forge.CheckState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.CheckState(nil), f.checks...)
}

func (f *previewFakeForge) getChangeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getChangeCalls
}

func (p previewFakeRunner) IndexSource(_ context.Context, request planner.SourceRequest) (planner.SourceEvidence, error) {
	evidence := p.evidence
	evidence.Mode = request.Mode
	if request.Mode == planner.SourceFinal {
		evidence.SourceSHA = request.FinalSHA
		evidence.PreviewIdentity = ""
		evidence.BaseSHA = ""
		evidence.HeadSHA = ""
	} else {
		evidence.SourceSHA = request.HeadSHA
		evidence.BaseSHA = request.BaseSHA
		evidence.HeadSHA = request.HeadSHA
	}
	for index := range evidence.Entities {
		evidence.Entities[index].SourceSHA = evidence.SourceSHA
	}
	for index := range evidence.Provenance {
		evidence.Provenance[index].SourceSHA = evidence.SourceSHA
	}
	evidence.EntityShapeDigest = domain.DigestSourceEvidence(evidence.Entities)
	return evidence, nil
}

func newTestCoordinator(t *testing.T) (*Coordinator, *CompositeStorage, *previewFakeForge, remote.Ref) {
	t.Helper()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	baseSHA := strings.Repeat("a", 40)
	headSHA := strings.Repeat("b", 40)
	entity := domain.Entity{Language: "go", Key: "example.Value", Kind: domain.EntityFunction, Name: "Value", Path: "main.go", StartLine: 1, EndLine: 1, SourceSHA: baseSHA, StructuralHash: strings.Repeat("c", 64)}
	object := domain.ContextObject{SchemaVersion: 3, RepositoryID: "context-repo", RefName: "refs/contexts/main", SourceSHA: baseSHA, Message: "base", Author: "test", CreatedAt: time.Now().UTC(), Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: baseSHA}}, Entities: []domain.Entity{entity}}
	contents, _ := json.Marshal(object)
	digest := blake3.Sum256(contents)
	objectID := formatDigest(digest[:])
	if _, err := store.PublishObject(context.Background(), "context-repo", objectID, contents); err != nil {
		t.Fatalf("PublishObject() error = %v", err)
	}
	canonicalRef := remote.Ref{RefName: "refs/contexts/main", CommitID: objectID, SourceSHA: baseSHA, Version: 1}
	if _, err := store.CompareAndSwapRef(context.Background(), "context-repo", canonicalRef.RefName, remote.Ref{RefName: canonicalRef.RefName}, canonicalRef); err != nil {
		t.Fatalf("CompareAndSwapRef() error = %v", err)
	}
	change := forge.Change{Key: domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}, State: forge.ChangeOpen, BaseRef: "main", BaseSHA: baseSHA, HeadRef: "feature", HeadSHA: headSHA}
	fakeForge := &previewFakeForge{change: change}
	target := entity
	target.SourceSHA = headSHA
	evidence := planner.SourceEvidence{RepositoryID: "context-repo", TargetRef: "refs/contexts/main", Mode: planner.SourcePreview, SourceSHA: headSHA, PreviewIdentity: strings.Repeat("d", 64), GitTreeDigest: strings.Repeat("e", 40), EntityShapeDigest: domain.DigestSourceEvidence([]domain.Entity{target}), Entities: []domain.Entity{target}, Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: headSHA}}, CoverageComplete: true, WorkerVersion: "test"}
	coordinator, err := NewCoordinator(CoordinatorConfig{Refs: store.refs, Objects: store, Forge: fakeForge, Runner: previewFakeRunner{evidence: evidence}, Repositories: []CoordinatorRepository{
		{RemoteKey: "repo", ContextRepositoryID: "context-repo", TargetRef: "refs/contexts/main", ForgeRepository: "owner/repository", InstallationID: 7, AutomaticLanding: true},
		{RemoteKey: "other", ContextRepositoryID: "other-context-repo", TargetRef: "refs/contexts/main", ForgeRepository: "owner/other", InstallationID: 7},
	}})
	if err != nil {
		t.Fatalf("NewCoordinator() error = %v", err)
	}
	return coordinator, store, fakeForge, canonicalRef
}

func formatDigest(value []byte) string {
	const digits = "0123456789abcdef"
	output := make([]byte, len(value)*2)
	for index, item := range value {
		output[index*2] = digits[item>>4]
		output[index*2+1] = digits[item&15]
	}
	return string(output)
}
