package backend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/tae2089/thread-keep/internal/remote/server"
)

type lifecycleFakeBackend struct {
	observation Observation
	ensures     int
	cancels     int
	ensureErr   error
}

func (f *lifecycleFakeBackend) Name() BackendName { return BackendDocker }
func (f *lifecycleFakeBackend) Adoptable() bool   { return true }
func (f *lifecycleFakeBackend) Ensure(_ context.Context, spec ExecutionSpec) (BackendHandle, error) {
	f.ensures++
	return BackendHandle{Version: 1, Backend: BackendDocker, ResourceID: "runner-container", ExecutionID: spec.ExecutionID, AttemptID: spec.AttemptID, SpecDigest: spec.SpecDigest}, f.ensureErr
}

func TestDurableSourceRunnerPersistsDiscoveredHandleWhenEnsureFails(t *testing.T) {
	store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
	if err != nil {
		t.Fatalf("OpenGormRefStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobID := strings.Repeat("9", 64)
	job := server.CoordinatorJob{ID: jobID, DedupeKey: "preview:ensure-failure", Kind: "preview_plan", Payload: []byte(`{"schema_version":1}`), State: server.CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: time.Now().Add(-time.Minute)}
	if created, err := store.EnqueueJob(context.Background(), job); err != nil || !created {
		t.Fatalf("EnqueueJob() = %t, %v", created, err)
	}
	claimed, ok, err := store.ClaimJob(context.Background(), "worker-1", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimJob() = %+v, %t, %v", claimed, ok, err)
	}
	request := planner.SourceRequest{Mode: planner.SourceFinal, RepositoryID: "repository", TargetRef: "refs/contexts/main", RepositoryURL: "https://github.com/owner/repository.git", Credential: "one-job-secret", FinalSHA: strings.Repeat("8", 40)}
	backend := &lifecycleFakeBackend{ensureErr: domain.NewError(domain.CodeBusy, errors.New("ambiguous credential delivery"))}
	runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-process-1", SpecDigest: strings.Repeat("7", 64)})
	if err != nil {
		t.Fatalf("NewDurableSourceRunner() error = %v", err)
	}
	if _, err := runner.IndexClaimedSource(context.Background(), planner.RunnerClaim{JobID: claimed.ID, LeaseOwner: claimed.LeaseOwner, FencingToken: claimed.FencingToken}, request); domain.CodeOf(err) != domain.CodeBusy {
		t.Fatalf("IndexClaimedSource() error = %v", err)
	}
	requestDigest, err := RequestDigest(request)
	if err != nil {
		t.Fatalf("RequestDigest() error = %v", err)
	}
	executionID, err := ExecutionID(jobID, requestDigest)
	if err != nil {
		t.Fatalf("ExecutionID() error = %v", err)
	}
	execution, err := store.RunnerExecution(context.Background(), executionID)
	if err != nil {
		t.Fatalf("RunnerExecution() error = %v", err)
	}
	if execution.State != server.RunnerExecutionFailed || len(execution.HandleEnvelope) == 0 || execution.CleanupState != server.RunnerCleanupPending {
		t.Fatalf("RunnerExecution() = %+v, want failed attempt with persisted handle and pending cleanup", execution)
	}
}
func (f *lifecycleFakeBackend) Observe(context.Context, BackendHandle) (Observation, error) {
	return f.observation, nil
}
func (f *lifecycleFakeBackend) Cancel(context.Context, BackendHandle) error {
	f.cancels++
	return nil
}
func (f *lifecycleFakeBackend) Cleanup(context.Context, BackendHandle) error { return nil }

func TestDurableSourceRunnerCompletesClaimedExecution(t *testing.T) {
	store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
	if err != nil {
		t.Fatalf("OpenGormRefStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobID := strings.Repeat("a", 64)
	job := server.CoordinatorJob{ID: jobID, DedupeKey: "preview:durable-runner", Kind: "preview_plan", Payload: []byte(`{"schema_version":1}`), State: server.CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: time.Now().Add(-time.Minute)}
	if created, err := store.EnqueueJob(context.Background(), job); err != nil || !created {
		t.Fatalf("EnqueueJob() = %t, %v", created, err)
	}
	claimed, ok, err := store.ClaimJob(context.Background(), "worker-1", time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimJob() = %+v, %t, %v", claimed, ok, err)
	}
	request := planner.SourceRequest{Mode: planner.SourceFinal, RepositoryID: "repository", TargetRef: "refs/contexts/main", RepositoryURL: "https://github.com/owner/repository.git", Credential: "one-job-secret", FinalSHA: strings.Repeat("b", 40)}
	evidence := planner.SourceEvidence{RepositoryID: request.RepositoryID, TargetRef: request.TargetRef, Mode: request.Mode, SourceSHA: request.FinalSHA, GitTreeDigest: strings.Repeat("c", 64), EntityShapeDigest: domain.DigestSourceEvidence(nil), Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "go", IndexerVersion: "1", SourceSHA: request.FinalSHA}}, CoverageComplete: true, WorkerVersion: planner.WorkerVersion}
	envelope, err := EncodeResult(ResultEnvelope{Version: 1, Evidence: evidence})
	if err != nil {
		t.Fatalf("EncodeResult() error = %v", err)
	}
	backend := &lifecycleFakeBackend{observation: Observation{State: ObservationSucceeded, ResultEnvelope: envelope}}
	runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-process-1", SpecDigest: strings.Repeat("d", 64), Wait: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatalf("NewDurableSourceRunner() error = %v", err)
	}
	got, err := runner.IndexClaimedSource(context.Background(), planner.RunnerClaim{JobID: claimed.ID, LeaseOwner: claimed.LeaseOwner, FencingToken: claimed.FencingToken}, request)
	if err != nil {
		t.Fatalf("IndexClaimedSource() error = %v", err)
	}
	if got.SourceSHA != evidence.SourceSHA || backend.ensures != 1 || backend.cancels != 0 {
		t.Fatalf("IndexClaimedSource() = %+v, ensures=%d cancels=%d", got, backend.ensures, backend.cancels)
	}
	requestDigest, err := RequestDigest(request)
	if err != nil {
		t.Fatalf("RequestDigest() error = %v", err)
	}
	executionID, err := ExecutionID(jobID, requestDigest)
	if err != nil {
		t.Fatalf("ExecutionID() error = %v", err)
	}
	execution, err := store.RunnerExecution(context.Background(), executionID)
	if err != nil || execution.State != server.RunnerExecutionSucceeded || execution.ResultDigest == "" || execution.CleanupState != server.RunnerCleanupPending {
		t.Fatalf("RunnerExecution() = %+v, %v", execution, err)
	}
}
