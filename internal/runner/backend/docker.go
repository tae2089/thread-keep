package backend

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
)

type DockerContainerState string

const (
	DockerContainerCreated DockerContainerState = "created"
	DockerContainerRunning DockerContainerState = "running"
	DockerContainerExited  DockerContainerState = "exited"

	DockerUploadRequest       DockerUploadKind = "request"
	DockerUploadCredential    DockerUploadKind = "credential"
	DockerUploadCompletionAck DockerUploadKind = "completion_ack"

	dockerRunnerUser               = "65532:65532"
	dockerRunnerPath               = "/usr/local/bin/thread-keep-runner"
	dockerRequestPath              = "/run/thread-keep-request/request.json"
	dockerCredentialPath           = "/run/thread-keep-secret/token"
	dockerResultPath               = "/run/thread-keep-result/result.json"
	dockerCompletionAckPath        = "/run/thread-keep-result/completion-ack"
	dockerLabelManaged             = "io.thread-keep.runner"
	dockerLabelExecutionID         = "io.thread-keep.runner.execution"
	dockerLabelAttemptID           = "io.thread-keep.runner.attempt"
	dockerLabelRequestDigest       = "io.thread-keep.runner.request"
	dockerLabelSpecDigest          = "io.thread-keep.runner.spec"
	defaultDockerMaxRequestBytes   = 1 << 20
	defaultDockerMaxResultBytes    = 16 << 20
	defaultDockerCredentialTimeout = 30 * time.Second
)

var (
	ErrDockerResourceNotFound  = errors.New("Docker resource not found")
	ErrDockerResultUnavailable = errors.New("Docker result is unavailable")
)

type DockerUploadKind string

type DockerContainer struct {
	ID       string
	State    DockerContainerState
	ExitCode int
	Labels   map[string]string
}

type DockerCreateSpec struct {
	Name                  string
	Image                 string
	Network               string
	User                  string
	CPULimitMillis        int64
	MemoryLimitBytes      int64
	WorkspaceLimitBytes   int64
	MaxRequestBytes       int
	MaxResultBytes        int
	CredentialWaitTimeout time.Duration
	ExecutionTimeout      time.Duration
	ReadonlyRootFS        bool
	Labels                map[string]string
}

type DockerUpload struct {
	Kind    DockerUploadKind
	Content []byte
}

type DockerEngine interface {
	Inspect(context.Context, string) (DockerContainer, error)
	Create(context.Context, DockerCreateSpec) (DockerContainer, error)
	Start(context.Context, string) error
	Upload(context.Context, string, DockerUpload) error
	Download(context.Context, string, int) ([]byte, error)
	Stop(context.Context, string) error
	Remove(context.Context, string) error
}

type DockerBackendConfig struct {
	Engine                DockerEngine
	Image                 string
	Network               string
	CPULimitMillis        int64
	MemoryLimitBytes      int64
	WorkspaceLimitBytes   int64
	MaxRequestBytes       int
	MaxResultBytes        int
	CredentialWaitTimeout time.Duration
}

type DockerBackend struct {
	engine                DockerEngine
	image                 string
	network               string
	cpuLimitMillis        int64
	memoryLimitBytes      int64
	workspaceLimitBytes   int64
	maxRequestBytes       int
	maxResultBytes        int
	credentialWaitTimeout time.Duration
}

func NewDockerBackend(config DockerBackendConfig) (*DockerBackend, error) {
	if config.Engine == nil || !digestPinnedRunnerImage(config.Image) || strings.TrimSpace(config.Network) == "" || config.CPULimitMillis <= 0 || config.MemoryLimitBytes <= 0 || config.WorkspaceLimitBytes <= 0 {
		return nil, backendValidationError("Docker backend configuration is invalid")
	}
	if config.MaxRequestBytes == 0 {
		config.MaxRequestBytes = defaultDockerMaxRequestBytes
	}
	if config.MaxResultBytes == 0 {
		config.MaxResultBytes = defaultDockerMaxResultBytes
	}
	if config.CredentialWaitTimeout == 0 {
		config.CredentialWaitTimeout = defaultDockerCredentialTimeout
	}
	if config.MaxRequestBytes < 1 || config.MaxResultBytes < 1 || config.CredentialWaitTimeout < time.Second {
		return nil, backendValidationError("Docker backend bounds are invalid")
	}
	return &DockerBackend{
		engine:                config.Engine,
		image:                 config.Image,
		network:               config.Network,
		cpuLimitMillis:        config.CPULimitMillis,
		memoryLimitBytes:      config.MemoryLimitBytes,
		workspaceLimitBytes:   config.WorkspaceLimitBytes,
		maxRequestBytes:       config.MaxRequestBytes,
		maxResultBytes:        config.MaxResultBytes,
		credentialWaitTimeout: config.CredentialWaitTimeout,
	}, nil
}

func (b *DockerBackend) Name() BackendName { return BackendDocker }

func (b *DockerBackend) Adoptable() bool { return true }

func (b *DockerBackend) Ensure(ctx context.Context, spec ExecutionSpec) (BackendHandle, error) {
	if err := validateExecutionSpec(spec); err != nil {
		return BackendHandle{}, err
	}
	name := dockerContainerName(spec.AttemptID)
	container, err := b.engine.Inspect(ctx, name)
	if err == nil {
		return b.resumeContainer(ctx, spec, container)
	}
	if !errors.Is(err, ErrDockerResourceNotFound) {
		return BackendHandle{}, dockerEngineError("inspect Docker runner", err)
	}
	labels := dockerLabels(spec)
	container, createErr := b.engine.Create(ctx, DockerCreateSpec{
		Name:                  name,
		Image:                 b.image,
		Network:               b.network,
		User:                  dockerRunnerUser,
		CPULimitMillis:        b.cpuLimitMillis,
		MemoryLimitBytes:      b.memoryLimitBytes,
		WorkspaceLimitBytes:   b.workspaceLimitBytes,
		MaxRequestBytes:       b.maxRequestBytes,
		MaxResultBytes:        b.maxResultBytes,
		CredentialWaitTimeout: b.credentialWaitTimeout,
		ExecutionTimeout:      spec.Timeout,
		ReadonlyRootFS:        true,
		Labels:                labels,
	})
	if createErr != nil {
		container, err = b.engine.Inspect(ctx, name)
		if err != nil {
			return BackendHandle{}, dockerEngineError("create Docker runner", createErr)
		}
	}
	return b.resumeContainer(ctx, spec, container)
}

func (b *DockerBackend) Observe(ctx context.Context, handle BackendHandle) (Observation, error) {
	if err := validateDockerHandle(handle); err != nil {
		return Observation{}, err
	}
	container, err := b.engine.Inspect(ctx, handle.ResourceID)
	if errors.Is(err, ErrDockerResourceNotFound) {
		return Observation{State: ObservationLost, FailureCode: domain.CodeCoverageIncomplete}, nil
	}
	if err != nil {
		return Observation{}, dockerEngineError("inspect Docker runner", err)
	}
	if err := validateDockerContainerHandle(container, handle); err != nil {
		return Observation{}, err
	}
	if container.State == DockerContainerRunning {
		result, err := b.engine.Download(ctx, container.ID, b.maxResultBytes)
		if errors.Is(err, ErrDockerResultUnavailable) {
			return Observation{State: ObservationRunning}, nil
		}
		if err != nil {
			return Observation{}, dockerEngineError("download Docker runner result", err)
		}
		if _, err := DecodeResult(result); err != nil {
			return Observation{}, err
		}
		if err := b.engine.Upload(ctx, container.ID, DockerUpload{Kind: DockerUploadCompletionAck}); err != nil {
			return Observation{}, dockerEngineError("acknowledge Docker runner result", err)
		}
		return Observation{State: ObservationSucceeded, ResultEnvelope: result}, nil
	}
	if container.State == DockerContainerCreated {
		return Observation{State: ObservationRunning}, nil
	}
	if container.State == DockerContainerExited && container.ExitCode != 0 {
		return Observation{State: ObservationFailed, FailureCode: domain.CodeCoverageIncomplete}, nil
	}
	return Observation{State: ObservationLost, FailureCode: domain.CodeCoverageIncomplete}, nil
}

func (b *DockerBackend) Cancel(ctx context.Context, handle BackendHandle) error {
	if err := validateDockerHandle(handle); err != nil {
		return err
	}
	err := b.engine.Stop(ctx, handle.ResourceID)
	if err == nil || errors.Is(err, ErrDockerResourceNotFound) {
		return nil
	}
	return dockerEngineError("stop Docker runner", err)
}

func (b *DockerBackend) Cleanup(ctx context.Context, handle BackendHandle) error {
	if err := validateDockerHandle(handle); err != nil {
		return err
	}
	err := b.engine.Remove(ctx, handle.ResourceID)
	if err == nil || errors.Is(err, ErrDockerResourceNotFound) {
		return nil
	}
	return dockerEngineError("remove Docker runner", err)
}

func (b *DockerBackend) CleanupDiscovered(ctx context.Context, identity CleanupIdentity) error {
	if err := validateCleanupIdentity(identity); err != nil {
		return err
	}
	container, err := b.engine.Inspect(ctx, dockerContainerName(identity.AttemptID))
	if errors.Is(err, ErrDockerResourceNotFound) {
		return nil
	}
	if err != nil {
		return dockerEngineError("inspect Docker runner for cleanup", err)
	}
	if err := validateDockerContainerSpec(container, ExecutionSpec{ExecutionID: identity.ExecutionID, AttemptID: identity.AttemptID, RequestDigest: identity.RequestDigest, SpecDigest: identity.SpecDigest}); err != nil {
		return err
	}
	err = b.engine.Remove(ctx, container.ID)
	if err == nil || errors.Is(err, ErrDockerResourceNotFound) {
		return nil
	}
	return dockerEngineError("remove discovered Docker runner", err)
}

func (b *DockerBackend) resumeContainer(ctx context.Context, spec ExecutionSpec, container DockerContainer) (BackendHandle, error) {
	if err := validateDockerContainerSpec(container, spec); err != nil {
		return BackendHandle{}, err
	}
	handle := dockerHandleForSpec(spec, container.ID)
	if container.State != DockerContainerCreated {
		return handle, nil
	}
	if err := b.engine.Start(ctx, container.ID); err != nil {
		return handle, dockerEngineError("start Docker runner", err)
	}
	request := spec.Request
	credential := []byte(request.Credential)
	request.Credential = ""
	if len(credential) == 0 {
		return handle, domain.NewError(domain.CodeAuth, errors.New("Docker runner credential is empty"))
	}
	defer clear(credential)
	requestBytes, err := json.Marshal(request)
	if err != nil || len(requestBytes) > b.maxRequestBytes {
		return handle, domain.NewError(domain.CodeCoverageIncomplete, errors.New("Docker runner request exceeds the limit"))
	}
	if err := b.engine.Upload(ctx, container.ID, DockerUpload{Kind: DockerUploadRequest, Content: requestBytes}); err != nil {
		return handle, dockerEngineError("upload Docker runner request", err)
	}
	if err := b.engine.Upload(ctx, container.ID, DockerUpload{Kind: DockerUploadCredential, Content: credential}); err != nil {
		return handle, dockerEngineError("upload Docker runner credential", err)
	}
	return handle, nil
}

func validateExecutionSpec(spec ExecutionSpec) error {
	if !validDigest(spec.ExecutionID) || !validDigest(spec.AttemptID) || !validDigest(spec.RequestDigest) || !validDigest(spec.SpecDigest) || spec.RunnerAttempt < 1 || spec.Timeout <= 0 {
		return backendValidationError("runner execution spec is invalid")
	}
	if err := planner.ValidateSourceRequest(spec.Request); err != nil {
		return err
	}
	return nil
}

func validateDockerContainerSpec(container DockerContainer, spec ExecutionSpec) error {
	if strings.TrimSpace(container.ID) == "" || container.Labels[dockerLabelManaged] != "true" || container.Labels[dockerLabelExecutionID] != spec.ExecutionID || container.Labels[dockerLabelAttemptID] != spec.AttemptID || container.Labels[dockerLabelRequestDigest] != spec.RequestDigest || container.Labels[dockerLabelSpecDigest] != spec.SpecDigest {
		return backendValidationError("Docker runner resource identity does not match the execution spec")
	}
	return nil
}

func validateDockerContainerHandle(container DockerContainer, handle BackendHandle) error {
	if strings.TrimSpace(container.ID) == "" || container.ID != handle.ResourceID || container.Labels[dockerLabelManaged] != "true" || container.Labels[dockerLabelExecutionID] != handle.ExecutionID || container.Labels[dockerLabelAttemptID] != handle.AttemptID || container.Labels[dockerLabelSpecDigest] != handle.SpecDigest {
		return backendValidationError("Docker runner resource identity does not match the stored handle")
	}
	return nil
}

func validateDockerHandle(handle BackendHandle) error {
	if handle.Version != 1 || handle.Backend != BackendDocker || strings.TrimSpace(handle.ResourceID) == "" || !validDigest(handle.ExecutionID) || !validDigest(handle.AttemptID) || !validDigest(handle.SpecDigest) {
		return backendValidationError("Docker runner handle is invalid")
	}
	return nil
}

func dockerHandleForSpec(spec ExecutionSpec, resourceID string) BackendHandle {
	return BackendHandle{Version: 1, Backend: BackendDocker, ResourceID: resourceID, ExecutionID: spec.ExecutionID, AttemptID: spec.AttemptID, RequestDigest: spec.RequestDigest, SpecDigest: spec.SpecDigest}
}

func dockerLabels(spec ExecutionSpec) map[string]string {
	return map[string]string{
		dockerLabelManaged:       "true",
		dockerLabelExecutionID:   spec.ExecutionID,
		dockerLabelAttemptID:     spec.AttemptID,
		dockerLabelRequestDigest: spec.RequestDigest,
		dockerLabelSpecDigest:    spec.SpecDigest,
	}
}

func dockerContainerName(attemptID string) string {
	return "thread-keep-runner-" + attemptID[:24]
}

func digestPinnedRunnerImage(image string) bool {
	if len(image) == len("sha256:")+64 && strings.HasPrefix(image, "sha256:") {
		_, err := hex.DecodeString(strings.TrimPrefix(image, "sha256:"))
		return err == nil
	}
	const marker = "@sha256:"
	index := strings.LastIndex(image, marker)
	if index <= 0 || index+len(marker)+64 != len(image) {
		return false
	}
	_, err := hex.DecodeString(image[index+len(marker):])
	return err == nil
}

func backendValidationError(message string) error {
	return domain.NewError(domain.CodeValidation, errors.New(message))
}

func dockerEngineError(operation string, err error) error {
	return domain.NewError(domain.CodeBusy, fmt.Errorf("%s: %w", operation, err))
}
