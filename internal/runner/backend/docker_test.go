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

type fakeDockerEngine struct {
	container      DockerContainer
	inspectErr     error
	createErr      error
	startErr       error
	uploadErr      map[DockerUploadKind]error
	download       []byte
	downloadErr    error
	stopErr        error
	removeErr      error
	createSpec     DockerCreateSpec
	uploads        []DockerUpload
	starts         int
	stops          int
	removes        int
	removeTargets  []string
	createdOnError bool
}

func (f *fakeDockerEngine) Inspect(context.Context, string) (DockerContainer, error) {
	if f.inspectErr != nil && f.container.ID == "" {
		return DockerContainer{}, f.inspectErr
	}
	return f.container, nil
}

func (f *fakeDockerEngine) Create(_ context.Context, spec DockerCreateSpec) (DockerContainer, error) {
	f.createSpec = spec
	if f.container.ID == "" {
		f.container = DockerContainer{ID: "container-1", State: DockerContainerCreated, Labels: spec.Labels}
	}
	if f.createErr != nil {
		if !f.createdOnError {
			f.container = DockerContainer{}
		}
		return DockerContainer{}, f.createErr
	}
	return f.container, nil
}

func (f *fakeDockerEngine) Start(context.Context, string) error {
	f.starts++
	if f.startErr == nil {
		f.container.State = DockerContainerRunning
	}
	return f.startErr
}

func (f *fakeDockerEngine) Upload(_ context.Context, _ string, upload DockerUpload) error {
	upload.Content = append([]byte(nil), upload.Content...)
	f.uploads = append(f.uploads, upload)
	return f.uploadErr[upload.Kind]
}

func (f *fakeDockerEngine) Download(context.Context, string, int) ([]byte, error) {
	return f.download, f.downloadErr
}

func (f *fakeDockerEngine) Stop(context.Context, string) error {
	f.stops++
	return f.stopErr
}

func (f *fakeDockerEngine) Remove(_ context.Context, target string) error {
	f.removes++
	f.removeTargets = append(f.removeTargets, target)
	return f.removeErr
}

func TestDockerBackendCreatesStartsAndDeliversCredentialOnce(t *testing.T) {
	engine := &fakeDockerEngine{inspectErr: ErrDockerResourceNotFound, uploadErr: make(map[DockerUploadKind]error)}
	backend := newTestDockerBackend(t, engine)
	spec := testDockerExecutionSpec()
	handle, err := backend.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if handle.ResourceID != "container-1" || handle.AttemptID != spec.AttemptID || handle.SpecDigest != spec.SpecDigest {
		t.Fatalf("Ensure() handle = %+v", handle)
	}
	if engine.starts != 1 || len(engine.uploads) != 2 || engine.uploads[0].Kind != DockerUploadRequest || engine.uploads[1].Kind != DockerUploadCredential {
		t.Fatalf("Ensure() starts=%d uploads=%+v", engine.starts, engine.uploads)
	}
	if strings.Contains(string(engine.uploads[0].Content), spec.Request.Credential) || string(engine.uploads[1].Content) != spec.Request.Credential {
		t.Fatal("Ensure() did not preserve the request/credential boundary")
	}
	if engine.createSpec.Image == "" || engine.createSpec.Labels[dockerLabelSpecDigest] != spec.SpecDigest || !engine.createSpec.ReadonlyRootFS || engine.createSpec.User != dockerRunnerUser {
		t.Fatalf("Create() spec = %+v", engine.createSpec)
	}
}

func TestDockerBackendAdoptsRunningContainerWithoutCredentialReinjection(t *testing.T) {
	spec := testDockerExecutionSpec()
	engine := &fakeDockerEngine{container: matchingDockerContainer(spec, DockerContainerRunning), uploadErr: make(map[DockerUploadKind]error)}
	backend := newTestDockerBackend(t, engine)
	if _, err := backend.Ensure(context.Background(), spec); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if engine.starts != 0 || len(engine.uploads) != 0 {
		t.Fatalf("Ensure() reinjected an adopted container: starts=%d uploads=%d", engine.starts, len(engine.uploads))
	}
}

func TestDockerBackendRejectsDeterministicNameCollision(t *testing.T) {
	spec := testDockerExecutionSpec()
	container := matchingDockerContainer(spec, DockerContainerRunning)
	container.Labels[dockerLabelSpecDigest] = strings.Repeat("f", 64)
	backend := newTestDockerBackend(t, &fakeDockerEngine{container: container, uploadErr: make(map[DockerUploadKind]error)})
	if _, err := backend.Ensure(context.Background(), spec); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Ensure() error = %v, want validation", err)
	}
}

func TestDockerBackendRediscoversCreateAfterLostResponse(t *testing.T) {
	spec := testDockerExecutionSpec()
	engine := &fakeDockerEngine{inspectErr: ErrDockerResourceNotFound, createErr: errors.New("response lost"), createdOnError: true, uploadErr: make(map[DockerUploadKind]error)}
	backend := newTestDockerBackend(t, engine)
	if _, err := backend.Ensure(context.Background(), spec); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if engine.starts != 1 || len(engine.uploads) != 2 {
		t.Fatalf("Ensure() did not resume the discovered create: starts=%d uploads=%d", engine.starts, len(engine.uploads))
	}
}

func TestDockerBackendReturnsHandleWhenCredentialDeliveryIsAmbiguous(t *testing.T) {
	engine := &fakeDockerEngine{inspectErr: ErrDockerResourceNotFound, uploadErr: map[DockerUploadKind]error{DockerUploadCredential: errors.New("response lost")}}
	backend := newTestDockerBackend(t, engine)
	handle, err := backend.Ensure(context.Background(), testDockerExecutionSpec())
	if err == nil || handle.ResourceID == "" {
		t.Fatalf("Ensure() = %+v, %v, want cleanup-capable handle and error", handle, err)
	}
	credentialUploads := 0
	for _, upload := range engine.uploads {
		if upload.Kind == DockerUploadCredential {
			credentialUploads++
		}
	}
	if credentialUploads != 1 {
		t.Fatalf("credential uploads = %d, want exactly one", credentialUploads)
	}
}

func TestDockerBackendObservesResultBeforeCompletionAck(t *testing.T) {
	spec := testDockerExecutionSpec()
	envelope, err := EncodeResult(ResultEnvelope{Version: 1, Code: domain.CodeCoverageIncomplete, Message: "safe"})
	if err != nil {
		t.Fatalf("EncodeResult() error = %v", err)
	}
	engine := &fakeDockerEngine{container: matchingDockerContainer(spec, DockerContainerRunning), download: envelope, uploadErr: make(map[DockerUploadKind]error)}
	backend := newTestDockerBackend(t, engine)
	observation, err := backend.Observe(context.Background(), dockerHandle(spec, engine.container.ID))
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.State != ObservationSucceeded || string(observation.ResultEnvelope) != string(envelope) || len(engine.uploads) != 1 || engine.uploads[0].Kind != DockerUploadCompletionAck {
		t.Fatalf("Observe() = %+v uploads=%+v", observation, engine.uploads)
	}
}

func TestDockerBackendObservesRunningAndLostStates(t *testing.T) {
	spec := testDockerExecutionSpec()
	engine := &fakeDockerEngine{container: matchingDockerContainer(spec, DockerContainerRunning), downloadErr: ErrDockerResultUnavailable, uploadErr: make(map[DockerUploadKind]error)}
	backend := newTestDockerBackend(t, engine)
	observation, err := backend.Observe(context.Background(), dockerHandle(spec, engine.container.ID))
	if err != nil || observation.State != ObservationRunning {
		t.Fatalf("Observe(running) = %+v, %v", observation, err)
	}
	engine.container.State = DockerContainerExited
	engine.container.ExitCode = 0
	observation, err = backend.Observe(context.Background(), dockerHandle(spec, engine.container.ID))
	if err != nil || observation.State != ObservationLost {
		t.Fatalf("Observe(exited) = %+v, %v", observation, err)
	}
}

func TestDockerBackendCancelAndCleanupAreNotFoundIdempotent(t *testing.T) {
	spec := testDockerExecutionSpec()
	engine := &fakeDockerEngine{container: matchingDockerContainer(spec, DockerContainerRunning), stopErr: ErrDockerResourceNotFound, removeErr: ErrDockerResourceNotFound, uploadErr: make(map[DockerUploadKind]error)}
	backend := newTestDockerBackend(t, engine)
	handle := dockerHandle(spec, engine.container.ID)
	if err := backend.Cancel(context.Background(), handle); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if err := backend.Cleanup(context.Background(), handle); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if engine.stops != 1 || engine.removes != 1 {
		t.Fatalf("stop/remove = %d/%d", engine.stops, engine.removes)
	}
}

func TestDurableSourceRunnerPreservesForeignDockerContainerDuringDiscoveryCleanup(t *testing.T) {
	store, err := server.OpenGormRefStore(t.TempDir() + "/runner.db")
	if err != nil {
		t.Fatalf("OpenGormRefStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	claim := claimLifecycleJob(t, store, "worker-1")
	spec := testDockerExecutionSpec()
	seed := server.RunnerExecutionSeed{ExecutionID: spec.ExecutionID, JobID: claim.JobID, RequestDigest: spec.RequestDigest, SpecDigest: spec.SpecDigest, Backend: string(BackendDocker), AttemptID: spec.AttemptID, OwnerInstance: "coordinator-1"}
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
	foreign := matchingDockerContainer(spec, DockerContainerRunning)
	foreign.Labels[dockerLabelSpecDigest] = strings.Repeat("f", 64)
	engine := &fakeDockerEngine{container: foreign, uploadErr: make(map[DockerUploadKind]error)}
	backend := newTestDockerBackend(t, engine)
	runner, err := NewDurableSourceRunner(DurableSourceRunnerConfig{Store: store, Backend: backend, InstanceID: "coordinator-1", SpecDigest: spec.SpecDigest})
	if err != nil {
		t.Fatalf("NewDurableSourceRunner() error = %v", err)
	}
	if err := runner.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if engine.removes != 0 {
		t.Fatalf("Remove() calls = %d, want foreign container preserved", engine.removes)
	}
	stored, err := store.RunnerExecution(context.Background(), execution.ExecutionID)
	if err != nil {
		t.Fatalf("RunnerExecution() error = %v", err)
	}
	if stored.CleanupState != server.RunnerCleanupFailed || stored.CleanupAttempts != 1 {
		t.Fatalf("RunnerExecution() cleanup = %q attempts=%d, want failed/1", stored.CleanupState, stored.CleanupAttempts)
	}
}

func TestDockerBackendDiscoveryCleanupRemovesValidatedContainerIDAndIsNotFoundIdempotent(t *testing.T) {
	spec := testDockerExecutionSpec()
	identity := CleanupIdentity{ExecutionID: spec.ExecutionID, AttemptID: spec.AttemptID, RequestDigest: spec.RequestDigest, SpecDigest: spec.SpecDigest}
	engine := &fakeDockerEngine{container: matchingDockerContainer(spec, DockerContainerRunning), uploadErr: make(map[DockerUploadKind]error)}
	backend := newTestDockerBackend(t, engine)
	if err := backend.CleanupDiscovered(context.Background(), identity); err != nil {
		t.Fatalf("CleanupDiscovered(existing) error = %v", err)
	}
	if len(engine.removeTargets) != 1 || engine.removeTargets[0] != engine.container.ID {
		t.Fatalf("Remove() targets = %+v, want validated container ID %q", engine.removeTargets, engine.container.ID)
	}
	missing := &fakeDockerEngine{inspectErr: ErrDockerResourceNotFound, uploadErr: make(map[DockerUploadKind]error)}
	backend = newTestDockerBackend(t, missing)
	if err := backend.CleanupDiscovered(context.Background(), identity); err != nil {
		t.Fatalf("CleanupDiscovered(missing) error = %v", err)
	}
	if missing.removes != 0 {
		t.Fatalf("Remove() calls for missing container = %d, want 0", missing.removes)
	}
}

func newTestDockerBackend(t *testing.T, engine DockerEngine) *DockerBackend {
	t.Helper()
	backend, err := NewDockerBackend(DockerBackendConfig{
		Engine:                engine,
		Image:                 "example.invalid/thread-keep-runner@sha256:" + strings.Repeat("a", 64),
		Network:               "none",
		CPULimitMillis:        250,
		MemoryLimitBytes:      64 << 20,
		WorkspaceLimitBytes:   256 << 20,
		MaxRequestBytes:       1 << 20,
		MaxResultBytes:        16 << 20,
		CredentialWaitTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDockerBackend() error = %v", err)
	}
	return backend
}

func testDockerExecutionSpec() ExecutionSpec {
	return ExecutionSpec{
		ExecutionID:   strings.Repeat("b", 64),
		AttemptID:     strings.Repeat("c", 64),
		RunnerAttempt: 1,
		RequestDigest: strings.Repeat("d", 64),
		SpecDigest:    strings.Repeat("e", 64),
		Timeout:       time.Minute,
		Request: planner.SourceRequest{
			Mode:          planner.SourceFinal,
			RepositoryID:  "repository",
			TargetRef:     "refs/contexts/main",
			RepositoryURL: "https://github.com/owner/repository.git",
			Credential:    "one-job-secret",
			FinalSHA:      strings.Repeat("1", 40),
		},
	}
}

func matchingDockerContainer(spec ExecutionSpec, state DockerContainerState) DockerContainer {
	return DockerContainer{
		ID:    "container-1",
		State: state,
		Labels: map[string]string{
			dockerLabelManaged:       "true",
			dockerLabelExecutionID:   spec.ExecutionID,
			dockerLabelAttemptID:     spec.AttemptID,
			dockerLabelRequestDigest: spec.RequestDigest,
			dockerLabelSpecDigest:    spec.SpecDigest,
		},
	}
}

func dockerHandle(spec ExecutionSpec, resourceID string) BackendHandle {
	return BackendHandle{Version: 1, Backend: BackendDocker, ResourceID: resourceID, ExecutionID: spec.ExecutionID, AttemptID: spec.AttemptID, SpecDigest: spec.SpecDigest}
}
