package app

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/tae2089/thread-keep/internal/runner/artifact"
	"github.com/tae2089/thread-keep/internal/runner/backend"
)

type BackendName string

const (
	BackendProcess       BackendName = "process"
	BackendInProcess     BackendName = "in_process"
	BackendDocker        BackendName = "docker"
	BackendKubernetesJob BackendName = "kubernetes_job"

	defaultReconcileIntervalSeconds = 30
	defaultMaxRequestBytes          = 1 << 20
	defaultMaxResultBytes           = 16 << 20
)

type RunnerConfig struct {
	Backend                  BackendName               `json:"backend,omitempty"`
	TimeoutSeconds           int                       `json:"timeout_seconds,omitempty"`
	ReconcileIntervalSeconds int                       `json:"reconcile_interval_seconds,omitempty"`
	Artifacts                RunnerArtifactConfig      `json:"artifacts,omitempty"`
	Process                  ProcessRunnerConfig       `json:"process,omitempty"`
	InProcess                InProcessRunnerConfig     `json:"in_process,omitempty"`
	Docker                   DockerRunnerConfig        `json:"docker,omitempty"`
	KubernetesJob            KubernetesJobRunnerConfig `json:"kubernetes_job,omitempty"`
	Timeout                  time.Duration             `json:"-"`
}

type RunnerArtifactConfig struct {
	Directory       string `json:"directory,omitempty"`
	MaxRequestBytes int    `json:"max_request_bytes,omitempty"`
	MaxResultBytes  int    `json:"max_result_bytes,omitempty"`
}

type ProcessRunnerConfig struct {
	Path string `json:"path,omitempty"`
}

type InProcessRunnerConfig struct {
	TempDir string `json:"temp_dir,omitempty"`
}

type DockerRunnerConfig struct {
	Endpoint            string `json:"endpoint,omitempty"`
	Image               string `json:"image,omitempty"`
	Network             string `json:"network,omitempty"`
	CPULimitMillis      int64  `json:"cpu_limit_millis,omitempty"`
	MemoryLimitBytes    int64  `json:"memory_limit_bytes,omitempty"`
	WorkspaceLimitBytes int64  `json:"workspace_limit_bytes,omitempty"`
	CleanupTTLSeconds   int    `json:"cleanup_ttl_seconds,omitempty"`
}

type KubernetesJobRunnerConfig struct {
	Image                   string `json:"image,omitempty"`
	Namespace               string `json:"namespace,omitempty"`
	JobServiceAccount       string `json:"job_service_account,omitempty"`
	ArtifactClaim           string `json:"artifact_claim,omitempty"`
	ArtifactFSGroup         int64  `json:"artifact_fs_group,omitempty"`
	CPURequestMillis        int64  `json:"cpu_request_millis,omitempty"`
	CPULimitMillis          int64  `json:"cpu_limit_millis,omitempty"`
	MemoryRequestBytes      int64  `json:"memory_request_bytes,omitempty"`
	MemoryLimitBytes        int64  `json:"memory_limit_bytes,omitempty"`
	TTLSecondsAfterFinished int    `json:"ttl_seconds_after_finished,omitempty"`
}

type RunnerDefaults struct {
	ProcessPath string
	Timeout     time.Duration
}

type RunnerOverrides struct {
	ProcessPath *string
	Timeout     *time.Duration
}

type runnerBuilder func(RunnerConfig) (planner.SourceRunner, error)

type runnerBuilders struct {
	process       runnerBuilder
	inProcess     runnerBuilder
	docker        runnerBuilder
	kubernetesJob runnerBuilder
}

type guardedSourceRunner struct {
	source planner.SourceRunner
}

func ResolveRunnerConfig(config RunnerConfig, defaults RunnerDefaults, overrides RunnerOverrides) (RunnerConfig, error) {
	if config.Backend == "" {
		config.Backend = BackendProcess
	}
	if config.TimeoutSeconds < 0 {
		return RunnerConfig{}, validationError("runner timeout_seconds must not be negative")
	}
	config.Timeout = defaults.Timeout
	if config.TimeoutSeconds > 0 {
		config.Timeout = time.Duration(config.TimeoutSeconds) * time.Second
		if int(config.Timeout/time.Second) != config.TimeoutSeconds {
			return RunnerConfig{}, validationError("runner timeout_seconds is too large")
		}
	}
	if overrides.Timeout != nil {
		config.Timeout = *overrides.Timeout
	}
	if config.Process.Path == "" {
		config.Process.Path = defaults.ProcessPath
	}
	if overrides.ProcessPath != nil {
		if config.Backend != BackendProcess {
			return RunnerConfig{}, validationError("--runner-path is valid only with the process backend")
		}
		config.Process.Path = *overrides.ProcessPath
	}
	if config.ReconcileIntervalSeconds == 0 {
		config.ReconcileIntervalSeconds = defaultReconcileIntervalSeconds
	}
	if config.Artifacts.MaxRequestBytes == 0 {
		config.Artifacts.MaxRequestBytes = defaultMaxRequestBytes
	}
	if config.Artifacts.MaxResultBytes == 0 {
		config.Artifacts.MaxResultBytes = defaultMaxResultBytes
	}
	if err := ValidateRunnerConfig(config); err != nil {
		return RunnerConfig{}, err
	}
	return config, nil
}

func ValidateRunnerConfig(config RunnerConfig) error {
	if config.Timeout <= 0 || config.ReconcileIntervalSeconds < 0 {
		return validationError("runner timeout must be positive and reconcile interval must not be negative")
	}
	switch config.Backend {
	case BackendProcess:
		if strings.TrimSpace(config.Process.Path) == "" {
			return validationError("process runner path is required")
		}
	case BackendInProcess:
		if config.InProcess.TempDir != "" && !filepath.IsAbs(config.InProcess.TempDir) {
			return validationError("in-process runner temp_dir must be absolute")
		}
	case BackendDocker:
		if strings.TrimSpace(config.Docker.Endpoint) == "" || !digestPinnedImage(config.Docker.Image) || strings.TrimSpace(config.Docker.Network) == "" || config.Docker.CPULimitMillis <= 0 || config.Docker.MemoryLimitBytes <= 0 || config.Docker.WorkspaceLimitBytes <= 0 || config.Docker.CleanupTTLSeconds <= 0 {
			return validationError("docker runner requires an explicit endpoint, digest-pinned image, network, resource/workspace limits, and cleanup TTL")
		}
	case BackendKubernetesJob:
		kubernetes := config.KubernetesJob
		if !digestPinnedImage(kubernetes.Image) || strings.TrimSpace(kubernetes.Namespace) == "" || strings.TrimSpace(kubernetes.JobServiceAccount) == "" || strings.TrimSpace(kubernetes.ArtifactClaim) == "" || kubernetes.ArtifactFSGroup <= 0 || kubernetes.CPULimitMillis <= 0 || kubernetes.MemoryLimitBytes <= 0 || kubernetes.TTLSecondsAfterFinished <= 0 || kubernetes.TTLSecondsAfterFinished > math.MaxInt32 || !filepath.IsAbs(config.Artifacts.Directory) || config.Artifacts.MaxRequestBytes <= 0 || config.Artifacts.MaxResultBytes <= 0 {
			return validationError("kubernetes runner requires a digest-pinned image, namespace, service account, artifact store, limits, and TTL")
		}
		if kubernetes.CPURequestMillis < 0 || kubernetes.CPURequestMillis > kubernetes.CPULimitMillis || kubernetes.MemoryRequestBytes < 0 || kubernetes.MemoryRequestBytes > kubernetes.MemoryLimitBytes {
			return validationError("kubernetes runner requests must not exceed limits")
		}
	default:
		return validationError("runner backend is not supported")
	}
	return nil
}

func BuildRunner(config RunnerConfig) (planner.SourceRunner, error) {
	return buildRunner(config, runnerBuilders{process: func(config RunnerConfig) (planner.SourceRunner, error) {
		return newProcessRunner(config.Process.Path, config.Timeout)
	}, inProcess: func(config RunnerConfig) (planner.SourceRunner, error) {
		return guardedSourceRunner{source: planner.NewNativeRunner(planner.NativeConfig{TempDir: config.InProcess.TempDir})}, nil
	}})
}

func BuildRunnerBackend(config RunnerConfig) (backend.RunnerBackend, planner.SourceRunner, error) {
	if err := ValidateRunnerConfig(config); err != nil {
		return nil, nil, err
	}
	switch config.Backend {
	case BackendProcess, BackendInProcess:
		runner, err := BuildRunner(config)
		if err != nil {
			return nil, nil, err
		}
		selectedBackend, err := backend.NewLocalBackend(backend.BackendName(config.Backend), runner)
		return selectedBackend, runner, err
	case BackendDocker:
		engine, err := backend.NewMobyDockerEngineFromEndpoint(config.Docker.Endpoint)
		if err != nil {
			return nil, nil, err
		}
		selectedBackend, err := backend.NewDockerBackend(backend.DockerBackendConfig{
			Engine:                engine,
			Image:                 config.Docker.Image,
			Network:               config.Docker.Network,
			CPULimitMillis:        config.Docker.CPULimitMillis,
			MemoryLimitBytes:      config.Docker.MemoryLimitBytes,
			WorkspaceLimitBytes:   config.Docker.WorkspaceLimitBytes,
			MaxRequestBytes:       config.Artifacts.MaxRequestBytes,
			MaxResultBytes:        config.Artifacts.MaxResultBytes,
			CredentialWaitTimeout: min(config.Timeout, 30*time.Second),
		})
		return selectedBackend, nil, err
	case BackendKubernetesJob:
		artifacts, err := artifact.NewFileStore(artifact.FileStoreConfig{Root: config.Artifacts.Directory, MaxRequestBytes: config.Artifacts.MaxRequestBytes, MaxResultBytes: config.Artifacts.MaxResultBytes})
		if err != nil {
			return nil, nil, err
		}
		client, err := backend.NewInClusterKubernetesClient()
		if err != nil {
			return nil, nil, err
		}
		selectedBackend, err := backend.NewKubernetesBackend(backend.KubernetesBackendConfig{
			Client:                  client,
			Artifacts:               artifacts,
			Namespace:               config.KubernetesJob.Namespace,
			Image:                   config.KubernetesJob.Image,
			JobServiceAccount:       config.KubernetesJob.JobServiceAccount,
			ArtifactClaim:           config.KubernetesJob.ArtifactClaim,
			ArtifactFSGroup:         config.KubernetesJob.ArtifactFSGroup,
			CPURequestMillis:        config.KubernetesJob.CPURequestMillis,
			CPULimitMillis:          config.KubernetesJob.CPULimitMillis,
			MemoryRequestBytes:      config.KubernetesJob.MemoryRequestBytes,
			MemoryLimitBytes:        config.KubernetesJob.MemoryLimitBytes,
			TTLSecondsAfterFinished: int32(config.KubernetesJob.TTLSecondsAfterFinished),
		})
		return selectedBackend, nil, err
	default:
		return nil, nil, validationError("selected runner backend is not available")
	}
}

func RunnerSpecDigest(config RunnerConfig) string {
	backendName := backend.BackendName(config.Backend)
	switch config.Backend {
	case BackendProcess:
		return backend.SpecDigest(backendName, config.Process.Path, config.Timeout.String())
	case BackendInProcess:
		return backend.SpecDigest(backendName, config.InProcess.TempDir, config.Timeout.String())
	case BackendDocker:
		return backend.SpecDigest(backendName,
			config.Docker.Endpoint,
			config.Docker.Image,
			config.Docker.Network,
			strconv.FormatInt(config.Docker.CPULimitMillis, 10),
			strconv.FormatInt(config.Docker.MemoryLimitBytes, 10),
			strconv.FormatInt(config.Docker.WorkspaceLimitBytes, 10),
			strconv.Itoa(config.Artifacts.MaxRequestBytes),
			strconv.Itoa(config.Artifacts.MaxResultBytes),
			config.Timeout.String(),
		)
	case BackendKubernetesJob:
		return backend.SpecDigest(backendName,
			config.KubernetesJob.Image,
			config.KubernetesJob.Namespace,
			config.KubernetesJob.JobServiceAccount,
			config.KubernetesJob.ArtifactClaim,
			strconv.FormatInt(config.KubernetesJob.ArtifactFSGroup, 10),
			strconv.FormatInt(config.KubernetesJob.CPURequestMillis, 10),
			strconv.FormatInt(config.KubernetesJob.CPULimitMillis, 10),
			strconv.FormatInt(config.KubernetesJob.MemoryRequestBytes, 10),
			strconv.FormatInt(config.KubernetesJob.MemoryLimitBytes, 10),
			strconv.Itoa(config.KubernetesJob.TTLSecondsAfterFinished),
			config.Artifacts.Directory,
			strconv.Itoa(config.Artifacts.MaxRequestBytes),
			strconv.Itoa(config.Artifacts.MaxResultBytes),
			config.Timeout.String(),
		)
	default:
		return backend.SpecDigest(backendName, config.Timeout.String())
	}
}

func RunnerCleanupDelay(config RunnerConfig) time.Duration {
	switch config.Backend {
	case BackendDocker:
		return time.Duration(config.Docker.CleanupTTLSeconds) * time.Second
	case BackendKubernetesJob:
		return time.Duration(config.KubernetesJob.TTLSecondsAfterFinished) * time.Second
	default:
		return 2 * time.Duration(config.ReconcileIntervalSeconds) * time.Second
	}
}

func buildRunner(config RunnerConfig, builders runnerBuilders) (planner.SourceRunner, error) {
	if err := ValidateRunnerConfig(config); err != nil {
		return nil, err
	}
	var builder runnerBuilder
	switch config.Backend {
	case BackendProcess:
		builder = builders.process
	case BackendInProcess:
		builder = builders.inProcess
	case BackendDocker:
		builder = builders.docker
	case BackendKubernetesJob:
		builder = builders.kubernetesJob
	}
	if builder == nil {
		return nil, validationError("selected runner backend is not available")
	}
	return builder(config)
}

func newProcessRunner(path string, timeout time.Duration) (planner.ProcessRunner, error) {
	resolvedPath, err := exec.LookPath(strings.TrimSpace(path))
	if err != nil {
		return planner.ProcessRunner{}, errors.New("coordinator requires an executable runner")
	}
	return planner.ProcessRunner{Path: resolvedPath, Timeout: timeout}, nil
}

func (g guardedSourceRunner) IndexSource(ctx context.Context, request planner.SourceRequest) (evidence planner.SourceEvidence, err error) {
	defer func() {
		if recover() != nil {
			evidence = planner.SourceEvidence{}
			err = domain.NewError(domain.CodeCoverageIncomplete, errors.New("in-process runner failed"))
		}
	}()
	return g.source.IndexSource(ctx, request)
}

func digestPinnedImage(image string) bool {
	const marker = "@sha256:"
	index := strings.LastIndex(image, marker)
	if index <= 0 || index+len(marker)+64 != len(image) {
		return false
	}
	_, err := hex.DecodeString(image[index+len(marker):])
	return err == nil
}

func validationError(message string) error {
	return domain.NewError(domain.CodeValidation, errors.New(message))
}
