package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/tae2089/thread-keep/internal/remote/server"
)

type lifecycleFakeBackend struct {
	name           BackendName
	observation    Observation
	ensures        int
	cancels        int
	observeHandles []BackendHandle
	cancelHandles  []BackendHandle
	cleanupHandles []BackendHandle
	cleanupIDs     []CleanupIdentity
	ensureErr      error
	requireRequest bool
}

type lifecycleSourceRunner struct{}

type handleIdentityCase struct {
	name   string
	mutate func(*BackendHandle)
}

func (f *lifecycleFakeBackend) Name() BackendName {
	if f.name == "" {
		return BackendDocker
	}
	return f.name
}
func (f *lifecycleFakeBackend) Adoptable() bool { return true }
func (f *lifecycleFakeBackend) Ensure(_ context.Context, spec ExecutionSpec) (BackendHandle, error) {
	f.ensures++
	return BackendHandle{Version: 1, Backend: f.Name(), ResourceID: "runner-container", ExecutionID: spec.ExecutionID, AttemptID: spec.AttemptID, RequestDigest: spec.RequestDigest, SpecDigest: spec.SpecDigest}, f.ensureErr
}
func (lifecycleSourceRunner) IndexSource(context.Context, planner.SourceRequest) (planner.SourceEvidence, error) {
	return planner.SourceEvidence{}, nil
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
func (f *lifecycleFakeBackend) Observe(_ context.Context, handle BackendHandle) (Observation, error) {
	f.observeHandles = append(f.observeHandles, handle)
	if f.requireRequest && !validDigest(handle.RequestDigest) {
		return Observation{}, backendValidationError("runner handle request identity is invalid")
	}
	return f.observation, nil
}
func (f *lifecycleFakeBackend) Cancel(_ context.Context, handle BackendHandle) error {
	f.cancels++
	f.cancelHandles = append(f.cancelHandles, handle)
	if f.requireRequest && !validDigest(handle.RequestDigest) {
		return backendValidationError("runner handle request identity is invalid")
	}
	return nil
}
func (f *lifecycleFakeBackend) Cleanup(_ context.Context, handle BackendHandle) error {
	f.cleanupHandles = append(f.cleanupHandles, handle)
	return nil
}
func (f *lifecycleFakeBackend) CleanupDiscovered(_ context.Context, identity CleanupIdentity) error {
	f.cleanupIDs = append(f.cleanupIDs, identity)
	return nil
}

func mismatchedHandleIdentityCases() []handleIdentityCase {
	return []handleIdentityCase{
		{name: "backend", mutate: func(handle *BackendHandle) { handle.Backend = BackendKubernetesJob }},
		{name: "execution ID", mutate: func(handle *BackendHandle) { handle.ExecutionID = strings.Repeat("a", 64) }},
		{name: "attempt ID", mutate: func(handle *BackendHandle) { handle.AttemptID = strings.Repeat("b", 64) }},
		{name: "request digest", mutate: func(handle *BackendHandle) { handle.RequestDigest = strings.Repeat("c", 64) }},
		{name: "spec digest", mutate: func(handle *BackendHandle) { handle.SpecDigest = strings.Repeat("d", 64) }},
	}
}

func mismatchedLocalHandleCases() []handleIdentityCase {
	return append([]handleIdentityCase{
		{name: "resource ID", mutate: func(handle *BackendHandle) { handle.ResourceID = strings.Repeat("f", 64) }},
	}, mismatchedHandleIdentityCases()...)
}

func missingExternalHandleIdentityCases() []handleIdentityCase {
	return []handleIdentityCase{
		{name: "execution ID", mutate: func(handle *BackendHandle) { handle.ExecutionID = "" }},
		{name: "attempt ID", mutate: func(handle *BackendHandle) { handle.AttemptID = "" }},
		{name: "spec digest", mutate: func(handle *BackendHandle) { handle.SpecDigest = "" }},
	}
}

func TestDurableSourceRunnerCleansDeterministicResourceWhenHandleWasNotPersisted(t *testing.T) {
	store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
	if err != nil {
		t.Fatalf("OpenGormRefStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	claim := claimLifecycleJob(t, store, "worker-1")
	seed := server.RunnerExecutionSeed{ExecutionID: strings.Repeat("2", 64), JobID: claim.JobID, RequestDigest: strings.Repeat("3", 64), SpecDigest: strings.Repeat("4", 64), Backend: string(BackendDocker), AttemptID: strings.Repeat("5", 64), OwnerInstance: "coordinator-1"}
	execution, err := store.PrepareRunnerExecution(context.Background(), claim, seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	if err := store.FailRunnerExecution(context.Background(), claim, execution.ExecutionID, execution.AttemptID, domain.CodeBusy); err != nil {
		t.Fatalf("FailRunnerExecution() error = %v", err)
	}
	if err := store.MarkRunnerCleanupPending(context.Background(), execution.ExecutionID, execution.AttemptID); err != nil {
		t.Fatalf("MarkRunnerCleanupPending() error = %v", err)
	}
	backend := &lifecycleFakeBackend{}
	runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest})
	if err != nil {
		t.Fatalf("NewDurableSourceRunner() error = %v", err)
	}
	if err := runner.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(backend.cleanupIDs) != 1 || backend.cleanupIDs[0] != cleanupIdentity(execution) {
		t.Fatalf("cleanup identities = %+v, want %+v", backend.cleanupIDs, cleanupIdentity(execution))
	}
}

func TestDurableSourceRunnerEnrichesLegacyCleanupHandleWithRequestDigest(t *testing.T) {
	store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
	if err != nil {
		t.Fatalf("OpenGormRefStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	claim := claimLifecycleJob(t, store, "worker-1")
	seed := server.RunnerExecutionSeed{ExecutionID: strings.Repeat("2", 64), JobID: claim.JobID, RequestDigest: strings.Repeat("3", 64), SpecDigest: strings.Repeat("4", 64), Backend: string(BackendDocker), AttemptID: strings.Repeat("5", 64), OwnerInstance: "coordinator-1"}
	execution, err := store.PrepareRunnerExecution(context.Background(), claim, seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	legacyHandle, err := json.Marshal(BackendHandle{Version: 1, Backend: BackendDocker, ResourceID: "container-1", ExecutionID: seed.ExecutionID, AttemptID: seed.AttemptID, SpecDigest: seed.SpecDigest})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := store.RecordRunnerHandle(context.Background(), claim, execution.ExecutionID, execution.AttemptID, legacyHandle); err != nil {
		t.Fatalf("RecordRunnerHandle() error = %v", err)
	}
	if err := store.FailRunnerExecution(context.Background(), claim, execution.ExecutionID, execution.AttemptID, domain.CodeBusy); err != nil {
		t.Fatalf("FailRunnerExecution() error = %v", err)
	}
	if err := store.MarkRunnerCleanupPending(context.Background(), execution.ExecutionID, execution.AttemptID); err != nil {
		t.Fatalf("MarkRunnerCleanupPending() error = %v", err)
	}
	backend := &lifecycleFakeBackend{}
	runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest})
	if err != nil {
		t.Fatalf("NewDurableSourceRunner() error = %v", err)
	}
	if err := runner.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(backend.cleanupHandles) != 1 || backend.cleanupHandles[0].RequestDigest != seed.RequestDigest {
		t.Fatalf("cleanup handles = %+v, want request digest %q", backend.cleanupHandles, seed.RequestDigest)
	}
}

func TestDurableSourceRunnerRejectsMismatchedPersistedHandleBeforeCleanup(t *testing.T) {
	for _, test := range mismatchedHandleIdentityCases() {
		t.Run(test.name, func(t *testing.T) {
			store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
			if err != nil {
				t.Fatalf("OpenGormRefStore() error = %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			claim := claimLifecycleJob(t, store, "worker-1")
			seed := server.RunnerExecutionSeed{ExecutionID: strings.Repeat("2", 64), JobID: claim.JobID, RequestDigest: strings.Repeat("3", 64), SpecDigest: strings.Repeat("4", 64), Backend: string(BackendDocker), AttemptID: strings.Repeat("5", 64), OwnerInstance: "coordinator-1"}
			handle := BackendHandle{Version: 1, Backend: BackendDocker, ResourceID: "container-1", ExecutionID: seed.ExecutionID, AttemptID: seed.AttemptID, RequestDigest: seed.RequestDigest, SpecDigest: seed.SpecDigest}
			test.mutate(&handle)
			execution := recordLifecycleHandle(t, store, claim, seed, handle)
			if err := store.FailRunnerExecution(context.Background(), claim, execution.ExecutionID, execution.AttemptID, domain.CodeBusy); err != nil {
				t.Fatalf("FailRunnerExecution() error = %v", err)
			}
			if err := store.MarkRunnerCleanupPending(context.Background(), execution.ExecutionID, execution.AttemptID); err != nil {
				t.Fatalf("MarkRunnerCleanupPending() error = %v", err)
			}
			backend := &lifecycleFakeBackend{}
			runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest})
			if err != nil {
				t.Fatalf("NewDurableSourceRunner() error = %v", err)
			}
			if err := runner.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if len(backend.cleanupHandles) != 0 {
				t.Fatalf("Cleanup() handles = %+v, want no backend call", backend.cleanupHandles)
			}
			stored, err := store.RunnerExecution(context.Background(), execution.ExecutionID)
			if err != nil {
				t.Fatalf("RunnerExecution() error = %v", err)
			}
			if stored.CleanupState != server.RunnerCleanupFailed || stored.CleanupAttempts != 1 {
				t.Fatalf("RunnerExecution() cleanup = %q attempts=%d, want failed/1", stored.CleanupState, stored.CleanupAttempts)
			}
		})
	}
}

func TestDurableSourceRunnerEnrichesLegacyHandleBeforeObserveAndCancel(t *testing.T) {
	store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
	if err != nil {
		t.Fatalf("OpenGormRefStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	claim := claimLifecycleJob(t, store, "worker-1")
	request := planner.SourceRequest{Mode: planner.SourceFinal, RepositoryID: "repository", TargetRef: "refs/contexts/main", RepositoryURL: "https://github.com/owner/repository.git", Credential: "one-job-secret", FinalSHA: strings.Repeat("8", 40)}
	requestDigest, err := RequestDigest(request)
	if err != nil {
		t.Fatalf("RequestDigest() error = %v", err)
	}
	executionID, err := ExecutionID(claim.JobID, requestDigest)
	if err != nil {
		t.Fatalf("ExecutionID() error = %v", err)
	}
	attemptID, err := AttemptID(executionID, 1)
	if err != nil {
		t.Fatalf("AttemptID() error = %v", err)
	}
	seed := server.RunnerExecutionSeed{ExecutionID: executionID, JobID: claim.JobID, RequestDigest: requestDigest, SpecDigest: strings.Repeat("4", 64), Backend: string(BackendDocker), AttemptID: attemptID, OwnerInstance: "coordinator-1"}
	execution, err := store.PrepareRunnerExecution(context.Background(), claim, seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	legacyHandle, err := json.Marshal(BackendHandle{Version: 1, Backend: BackendDocker, ResourceID: "container-1", ExecutionID: executionID, AttemptID: attemptID, SpecDigest: seed.SpecDigest})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := store.RecordRunnerHandle(context.Background(), claim, execution.ExecutionID, execution.AttemptID, legacyHandle); err != nil {
		t.Fatalf("RecordRunnerHandle() error = %v", err)
	}
	backend := &lifecycleFakeBackend{observation: Observation{State: ObservationRunning}, requireRequest: true}
	runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest, Wait: func(context.Context) error { return context.Canceled }})
	if err != nil {
		t.Fatalf("NewDurableSourceRunner() error = %v", err)
	}
	_, err = runner.IndexClaimedSource(context.Background(), planner.RunnerClaim{JobID: claim.JobID, LeaseOwner: claim.LeaseOwner, FencingToken: claim.FencingToken}, request)
	if domain.CodeOf(err) != domain.CodeBusy {
		t.Fatalf("IndexClaimedSource() error = %v, want busy cancellation", err)
	}
	if len(backend.observeHandles) != 1 || backend.observeHandles[0].RequestDigest != requestDigest {
		t.Fatalf("Observe() handles = %+v, want request digest %q", backend.observeHandles, requestDigest)
	}
	if len(backend.cancelHandles) != 1 || backend.cancelHandles[0].RequestDigest != requestDigest {
		t.Fatalf("Cancel() handles = %+v, want request digest %q", backend.cancelHandles, requestDigest)
	}
	stored, err := store.RunnerExecution(context.Background(), executionID)
	if err != nil {
		t.Fatalf("RunnerExecution() error = %v", err)
	}
	if stored.State != server.RunnerExecutionCancelled {
		t.Fatalf("RunnerExecution() state = %q, want %q", stored.State, server.RunnerExecutionCancelled)
	}
}

func TestDurableSourceRunnerRejectsMismatchedPersistedHandleBeforeActiveBackendCalls(t *testing.T) {
	for _, test := range mismatchedHandleIdentityCases() {
		t.Run(test.name, func(t *testing.T) {
			store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
			if err != nil {
				t.Fatalf("OpenGormRefStore() error = %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			claim := claimLifecycleJob(t, store, "worker-1")
			request := planner.SourceRequest{Mode: planner.SourceFinal, RepositoryID: "repository", TargetRef: "refs/contexts/main", RepositoryURL: "https://github.com/owner/repository.git", Credential: "one-job-secret", FinalSHA: strings.Repeat("8", 40)}
			requestDigest, err := RequestDigest(request)
			if err != nil {
				t.Fatalf("RequestDigest() error = %v", err)
			}
			executionID, err := ExecutionID(claim.JobID, requestDigest)
			if err != nil {
				t.Fatalf("ExecutionID() error = %v", err)
			}
			attemptID, err := AttemptID(executionID, 1)
			if err != nil {
				t.Fatalf("AttemptID() error = %v", err)
			}
			seed := server.RunnerExecutionSeed{ExecutionID: executionID, JobID: claim.JobID, RequestDigest: requestDigest, SpecDigest: strings.Repeat("4", 64), Backend: string(BackendDocker), AttemptID: attemptID, OwnerInstance: "coordinator-1"}
			handle := BackendHandle{Version: 1, Backend: BackendDocker, ResourceID: "container-1", ExecutionID: executionID, AttemptID: attemptID, RequestDigest: requestDigest, SpecDigest: seed.SpecDigest}
			test.mutate(&handle)
			recordLifecycleHandle(t, store, claim, seed, handle)
			backend := &lifecycleFakeBackend{observation: Observation{State: ObservationRunning}}
			runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest, Wait: func(context.Context) error { return context.Canceled }})
			if err != nil {
				t.Fatalf("NewDurableSourceRunner() error = %v", err)
			}
			_, err = runner.IndexClaimedSource(context.Background(), planner.RunnerClaim{JobID: claim.JobID, LeaseOwner: claim.LeaseOwner, FencingToken: claim.FencingToken}, request)
			if domain.CodeOf(err) != domain.CodeValidation {
				t.Fatalf("IndexClaimedSource() error = %v, want validation", err)
			}
			if len(backend.observeHandles) != 0 || len(backend.cancelHandles) != 0 {
				t.Fatalf("backend handles observe=%+v cancel=%+v, want no backend calls", backend.observeHandles, backend.cancelHandles)
			}
		})
	}
}

func TestDurableSourceRunnerEnrichesLocalLegacyHandleBeforeCleanup(t *testing.T) {
	for _, backendName := range []BackendName{BackendProcess, BackendInProcess} {
		t.Run(string(backendName), func(t *testing.T) {
			store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
			if err != nil {
				t.Fatalf("OpenGormRefStore() error = %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			claim := claimLifecycleJob(t, store, "worker-1")
			seed := server.RunnerExecutionSeed{ExecutionID: strings.Repeat("2", 64), JobID: claim.JobID, RequestDigest: strings.Repeat("3", 64), SpecDigest: strings.Repeat("4", 64), Backend: string(backendName), AttemptID: strings.Repeat("5", 64), OwnerInstance: "coordinator-1"}
			legacyHandle := BackendHandle{Version: 1, Backend: backendName, ResourceID: seed.AttemptID}
			execution := recordLifecycleHandle(t, store, claim, seed, legacyHandle)
			if err := store.FailRunnerExecution(context.Background(), claim, execution.ExecutionID, execution.AttemptID, domain.CodeBusy); err != nil {
				t.Fatalf("FailRunnerExecution() error = %v", err)
			}
			if err := store.MarkRunnerCleanupPending(context.Background(), execution.ExecutionID, execution.AttemptID); err != nil {
				t.Fatalf("MarkRunnerCleanupPending() error = %v", err)
			}
			backend := &lifecycleFakeBackend{name: backendName}
			runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest})
			if err != nil {
				t.Fatalf("NewDurableSourceRunner() error = %v", err)
			}
			if err := runner.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			want := BackendHandle{Version: 1, Backend: backendName, ResourceID: seed.AttemptID, ExecutionID: seed.ExecutionID, AttemptID: seed.AttemptID, RequestDigest: seed.RequestDigest, SpecDigest: seed.SpecDigest}
			if len(backend.cleanupHandles) != 1 || backend.cleanupHandles[0] != want {
				t.Fatalf("Cleanup() handles = %+v, want %+v", backend.cleanupHandles, want)
			}
		})
	}
}

func TestDurableSourceRunnerEnrichesLocalLegacyHandleBeforeObserveAndCancel(t *testing.T) {
	for _, backendName := range []BackendName{BackendProcess, BackendInProcess} {
		t.Run(string(backendName), func(t *testing.T) {
			store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
			if err != nil {
				t.Fatalf("OpenGormRefStore() error = %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			claim := claimLifecycleJob(t, store, "worker-1")
			request := planner.SourceRequest{Mode: planner.SourceFinal, RepositoryID: "repository", TargetRef: "refs/contexts/main", RepositoryURL: "https://github.com/owner/repository.git", Credential: "one-job-secret", FinalSHA: strings.Repeat("8", 40)}
			requestDigest, err := RequestDigest(request)
			if err != nil {
				t.Fatalf("RequestDigest() error = %v", err)
			}
			executionID, err := ExecutionID(claim.JobID, requestDigest)
			if err != nil {
				t.Fatalf("ExecutionID() error = %v", err)
			}
			attemptID, err := AttemptID(executionID, 1)
			if err != nil {
				t.Fatalf("AttemptID() error = %v", err)
			}
			seed := server.RunnerExecutionSeed{ExecutionID: executionID, JobID: claim.JobID, RequestDigest: requestDigest, SpecDigest: strings.Repeat("4", 64), Backend: string(backendName), AttemptID: attemptID, OwnerInstance: "coordinator-1"}
			legacyHandle := BackendHandle{Version: 1, Backend: backendName, ResourceID: attemptID}
			recordLifecycleHandle(t, store, claim, seed, legacyHandle)
			backend := &lifecycleFakeBackend{name: backendName, observation: Observation{State: ObservationRunning}}
			runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest, Wait: func(context.Context) error { return context.Canceled }})
			if err != nil {
				t.Fatalf("NewDurableSourceRunner() error = %v", err)
			}
			_, err = runner.IndexClaimedSource(context.Background(), planner.RunnerClaim{JobID: claim.JobID, LeaseOwner: claim.LeaseOwner, FencingToken: claim.FencingToken}, request)
			if domain.CodeOf(err) != domain.CodeBusy {
				t.Fatalf("IndexClaimedSource() error = %v, want busy cancellation", err)
			}
			want := BackendHandle{Version: 1, Backend: backendName, ResourceID: attemptID, ExecutionID: executionID, AttemptID: attemptID, RequestDigest: requestDigest, SpecDigest: seed.SpecDigest}
			if len(backend.observeHandles) != 1 || backend.observeHandles[0] != want {
				t.Fatalf("Observe() handles = %+v, want %+v", backend.observeHandles, want)
			}
			if len(backend.cancelHandles) != 1 || backend.cancelHandles[0] != want {
				t.Fatalf("Cancel() handles = %+v, want %+v", backend.cancelHandles, want)
			}
		})
	}
}

func TestDurableSourceRunnerRejectsMismatchedLocalHandleBeforeCleanup(t *testing.T) {
	for _, backendName := range []BackendName{BackendProcess, BackendInProcess} {
		for _, test := range mismatchedLocalHandleCases() {
			t.Run(string(backendName)+"/"+test.name, func(t *testing.T) {
				store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
				if err != nil {
					t.Fatalf("OpenGormRefStore() error = %v", err)
				}
				t.Cleanup(func() { _ = store.Close() })
				claim := claimLifecycleJob(t, store, "worker-1")
				seed := server.RunnerExecutionSeed{ExecutionID: strings.Repeat("2", 64), JobID: claim.JobID, RequestDigest: strings.Repeat("3", 64), SpecDigest: strings.Repeat("4", 64), Backend: string(backendName), AttemptID: strings.Repeat("5", 64), OwnerInstance: "coordinator-1"}
				handle := BackendHandle{Version: 1, Backend: backendName, ResourceID: seed.AttemptID, ExecutionID: seed.ExecutionID, AttemptID: seed.AttemptID, RequestDigest: seed.RequestDigest, SpecDigest: seed.SpecDigest}
				test.mutate(&handle)
				execution := recordLifecycleHandle(t, store, claim, seed, handle)
				if err := store.FailRunnerExecution(context.Background(), claim, execution.ExecutionID, execution.AttemptID, domain.CodeBusy); err != nil {
					t.Fatalf("FailRunnerExecution() error = %v", err)
				}
				if err := store.MarkRunnerCleanupPending(context.Background(), execution.ExecutionID, execution.AttemptID); err != nil {
					t.Fatalf("MarkRunnerCleanupPending() error = %v", err)
				}
				backend := &lifecycleFakeBackend{name: backendName}
				runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest})
				if err != nil {
					t.Fatalf("NewDurableSourceRunner() error = %v", err)
				}
				if err := runner.Reconcile(context.Background()); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				if len(backend.cleanupHandles) != 0 {
					t.Fatalf("Cleanup() handles = %+v, want no backend call", backend.cleanupHandles)
				}
			})
		}
	}
}

func TestDurableSourceRunnerRejectsMismatchedLocalHandleBeforeActiveBackendCalls(t *testing.T) {
	for _, backendName := range []BackendName{BackendProcess, BackendInProcess} {
		for _, test := range mismatchedLocalHandleCases() {
			t.Run(string(backendName)+"/"+test.name, func(t *testing.T) {
				store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
				if err != nil {
					t.Fatalf("OpenGormRefStore() error = %v", err)
				}
				t.Cleanup(func() { _ = store.Close() })
				claim := claimLifecycleJob(t, store, "worker-1")
				request := planner.SourceRequest{Mode: planner.SourceFinal, RepositoryID: "repository", TargetRef: "refs/contexts/main", RepositoryURL: "https://github.com/owner/repository.git", Credential: "one-job-secret", FinalSHA: strings.Repeat("8", 40)}
				requestDigest, err := RequestDigest(request)
				if err != nil {
					t.Fatalf("RequestDigest() error = %v", err)
				}
				executionID, err := ExecutionID(claim.JobID, requestDigest)
				if err != nil {
					t.Fatalf("ExecutionID() error = %v", err)
				}
				attemptID, err := AttemptID(executionID, 1)
				if err != nil {
					t.Fatalf("AttemptID() error = %v", err)
				}
				seed := server.RunnerExecutionSeed{ExecutionID: executionID, JobID: claim.JobID, RequestDigest: requestDigest, SpecDigest: strings.Repeat("4", 64), Backend: string(backendName), AttemptID: attemptID, OwnerInstance: "coordinator-1"}
				handle := BackendHandle{Version: 1, Backend: backendName, ResourceID: attemptID, ExecutionID: executionID, AttemptID: attemptID, RequestDigest: requestDigest, SpecDigest: seed.SpecDigest}
				test.mutate(&handle)
				recordLifecycleHandle(t, store, claim, seed, handle)
				backend := &lifecycleFakeBackend{name: backendName, observation: Observation{State: ObservationRunning}}
				runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest, Wait: func(context.Context) error { return context.Canceled }})
				if err != nil {
					t.Fatalf("NewDurableSourceRunner() error = %v", err)
				}
				_, err = runner.IndexClaimedSource(context.Background(), planner.RunnerClaim{JobID: claim.JobID, LeaseOwner: claim.LeaseOwner, FencingToken: claim.FencingToken}, request)
				if domain.CodeOf(err) != domain.CodeValidation {
					t.Fatalf("IndexClaimedSource() error = %v, want validation", err)
				}
				if len(backend.observeHandles) != 0 || len(backend.cancelHandles) != 0 {
					t.Fatalf("backend handles observe=%+v cancel=%+v, want no backend calls", backend.observeHandles, backend.cancelHandles)
				}
			})
		}
	}
}

func TestEnrichBackendHandleRejectsMissingExternalIdentity(t *testing.T) {
	execution := server.RunnerExecution{ExecutionID: strings.Repeat("2", 64), AttemptID: strings.Repeat("5", 64), RequestDigest: strings.Repeat("3", 64), SpecDigest: strings.Repeat("4", 64)}
	for _, backendName := range []BackendName{BackendDocker, BackendKubernetesJob} {
		execution.Backend = string(backendName)
		for _, test := range missingExternalHandleIdentityCases() {
			t.Run(string(backendName)+"/"+test.name, func(t *testing.T) {
				handle := BackendHandle{Version: 1, Backend: backendName, ResourceID: "external-resource", ExecutionID: execution.ExecutionID, AttemptID: execution.AttemptID, RequestDigest: execution.RequestDigest, SpecDigest: execution.SpecDigest}
				test.mutate(&handle)
				if _, err := enrichBackendHandle(handle, execution); domain.CodeOf(err) != domain.CodeValidation {
					t.Fatalf("enrichBackendHandle() error = %v, want validation", err)
				}
			})
		}
	}
}

func TestDurableSourceRunnerCleansLocalResourceWhenHandleWasNotPersisted(t *testing.T) {
	for _, backendName := range []BackendName{BackendProcess, BackendInProcess} {
		t.Run(string(backendName), func(t *testing.T) {
			store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
			if err != nil {
				t.Fatalf("OpenGormRefStore() error = %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			claim := claimLifecycleJob(t, store, "worker-1")
			seed := server.RunnerExecutionSeed{ExecutionID: strings.Repeat("2", 64), JobID: claim.JobID, RequestDigest: strings.Repeat("3", 64), SpecDigest: strings.Repeat("4", 64), Backend: string(backendName), AttemptID: strings.Repeat("5", 64), OwnerInstance: "coordinator-1"}
			execution, err := store.PrepareRunnerExecution(context.Background(), claim, seed)
			if err != nil {
				t.Fatalf("PrepareRunnerExecution() error = %v", err)
			}
			if err := store.FailRunnerExecution(context.Background(), claim, execution.ExecutionID, execution.AttemptID, domain.CodeBusy); err != nil {
				t.Fatalf("FailRunnerExecution() error = %v", err)
			}
			if err := store.MarkRunnerCleanupPending(context.Background(), execution.ExecutionID, execution.AttemptID); err != nil {
				t.Fatalf("MarkRunnerCleanupPending() error = %v", err)
			}
			backend, err := NewLocalBackend(backendName, lifecycleSourceRunner{})
			if err != nil {
				t.Fatalf("NewLocalBackend() error = %v", err)
			}
			handle, err := backend.Ensure(context.Background(), ExecutionSpec{AttemptID: execution.AttemptID})
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: seed.SpecDigest})
			if err != nil {
				t.Fatalf("NewDurableSourceRunner() error = %v", err)
			}
			if err := runner.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			observation, err := backend.Observe(context.Background(), handle)
			if err != nil {
				t.Fatalf("Observe() error = %v", err)
			}
			if observation.State != ObservationLost {
				t.Fatalf("Observe() state = %q, want %q after cleanup", observation.State, ObservationLost)
			}
			stored, err := store.RunnerExecution(context.Background(), execution.ExecutionID)
			if err != nil {
				t.Fatalf("RunnerExecution() error = %v", err)
			}
			if stored.CleanupState != server.RunnerCleanupCleaned {
				t.Fatalf("RunnerExecution() cleanup state = %q, want %q", stored.CleanupState, server.RunnerCleanupCleaned)
			}
		})
	}
}

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

func claimLifecycleJob(t *testing.T, store *server.GormRefStore, owner string) server.JobClaim {
	t.Helper()
	job := server.CoordinatorJob{ID: strings.Repeat("1", 64), DedupeKey: "preview:cleanup-recovery", Kind: "preview_plan", Payload: []byte(`{"schema_version":1}`), State: server.CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: time.Now().Add(-time.Minute)}
	if created, err := store.EnqueueJob(context.Background(), job); err != nil || !created {
		t.Fatalf("EnqueueJob() = %t, %v", created, err)
	}
	claimed, ok, err := store.ClaimJob(context.Background(), owner, time.Now().UTC(), time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimJob() = %+v, %t, %v", claimed, ok, err)
	}
	return claimed.Claim()
}

func recordLifecycleHandle(t *testing.T, store *server.GormRefStore, claim server.JobClaim, seed server.RunnerExecutionSeed, handle BackendHandle) server.RunnerExecution {
	t.Helper()
	execution, err := store.PrepareRunnerExecution(context.Background(), claim, seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	envelope, err := json.Marshal(handle)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := store.RecordRunnerHandle(context.Background(), claim, execution.ExecutionID, execution.AttemptID, envelope); err != nil {
		t.Fatalf("RecordRunnerHandle() error = %v", err)
	}
	return execution
}
