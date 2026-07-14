package app

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
)

type namedSourceRunner struct {
	name string
}

type panicSourceRunner struct{}

func (n namedSourceRunner) IndexSource(context.Context, planner.SourceRequest) (planner.SourceEvidence, error) {
	return planner.SourceEvidence{}, errors.New(n.name)
}

func (panicSourceRunner) IndexSource(context.Context, planner.SourceRequest) (planner.SourceEvidence, error) {
	panic("credential-shaped panic detail")
}

func TestResolveRunnerConfigDefaultsAndExplicitOverrides(t *testing.T) {
	configured := RunnerConfig{TimeoutSeconds: 41, Process: ProcessRunnerConfig{Path: "/configured/runner"}}
	resolved, err := ResolveRunnerConfig(configured, RunnerDefaults{ProcessPath: "thread-keep-runner", Timeout: 2 * time.Minute}, RunnerOverrides{})
	if err != nil {
		t.Fatalf("ResolveRunnerConfig() error = %v", err)
	}
	if resolved.Backend != BackendProcess || resolved.Process.Path != "/configured/runner" || resolved.Timeout != 41*time.Second {
		t.Fatalf("ResolveRunnerConfig() = %+v", resolved)
	}

	path := "/explicit/runner"
	timeout := 53 * time.Second
	resolved, err = ResolveRunnerConfig(configured, RunnerDefaults{ProcessPath: "thread-keep-runner", Timeout: 2 * time.Minute}, RunnerOverrides{ProcessPath: &path, Timeout: &timeout})
	if err != nil {
		t.Fatalf("ResolveRunnerConfig(overrides) error = %v", err)
	}
	if resolved.Process.Path != path || resolved.Timeout != timeout {
		t.Fatalf("ResolveRunnerConfig(overrides) = %+v", resolved)
	}
}

func TestBuildRunnerSelectsSupportedBackend(t *testing.T) {
	builders := runnerBuilders{
		process:   func(RunnerConfig) (planner.SourceRunner, error) { return namedSourceRunner{name: "process"}, nil },
		inProcess: func(RunnerConfig) (planner.SourceRunner, error) { return namedSourceRunner{name: "in_process"}, nil },
		docker:    func(RunnerConfig) (planner.SourceRunner, error) { return namedSourceRunner{name: "docker"}, nil },
		kubernetesJob: func(RunnerConfig) (planner.SourceRunner, error) {
			return namedSourceRunner{name: "kubernetes_job"}, nil
		},
	}
	tests := []struct {
		name   string
		config RunnerConfig
	}{
		{name: "process", config: RunnerConfig{Backend: BackendProcess, Timeout: time.Minute, Process: ProcessRunnerConfig{Path: "runner"}}},
		{name: "in_process", config: RunnerConfig{Backend: BackendInProcess, Timeout: time.Minute}},
		{name: "docker", config: RunnerConfig{Backend: BackendDocker, Timeout: time.Minute, Docker: DockerRunnerConfig{Endpoint: "unix:///run/docker.sock", Image: "registry.invalid/thread-keep@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Network: "none", CPULimitMillis: 1, MemoryLimitBytes: 1, WorkspaceLimitBytes: 1, CleanupTTLSeconds: 1}}},
		{name: "kubernetes_job", config: RunnerConfig{Backend: BackendKubernetesJob, Timeout: time.Minute, Artifacts: RunnerArtifactConfig{Directory: "/runner-artifacts", MaxRequestBytes: 1, MaxResultBytes: 1}, KubernetesJob: KubernetesJobRunnerConfig{Image: "registry.invalid/thread-keep@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Namespace: "thread-keep", JobServiceAccount: "runner", ArtifactClaim: "artifacts", ArtifactFSGroup: 65532, CPULimitMillis: 1, MemoryLimitBytes: 1, TTLSecondsAfterFinished: 1}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, err := buildRunner(test.config, builders)
			if err != nil {
				t.Fatalf("buildRunner() error = %v", err)
			}
			_, err = runner.IndexSource(context.Background(), planner.SourceRequest{})
			if err == nil || err.Error() != test.name {
				t.Fatalf("selected runner error = %v, want %q", err, test.name)
			}
		})
	}
}

func TestRunnerConfigRejectsUnknownBackendAndMissingSelectedRequirements(t *testing.T) {
	tests := []RunnerConfig{
		{Backend: "unknown", Timeout: time.Minute},
		{Backend: BackendProcess, Timeout: time.Minute},
		{Backend: BackendDocker, Timeout: time.Minute, Docker: DockerRunnerConfig{Endpoint: "unix:///run/docker.sock", Image: "mutable:latest", CPULimitMillis: 1, MemoryLimitBytes: 1, WorkspaceLimitBytes: 1, CleanupTTLSeconds: 1}},
		{Backend: BackendKubernetesJob, Timeout: time.Minute, KubernetesJob: KubernetesJobRunnerConfig{Image: "registry.invalid/thread-keep@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},
	}
	for index, config := range tests {
		if err := ValidateRunnerConfig(config); err == nil {
			t.Errorf("ValidateRunnerConfig(test %d) error = nil", index)
		}
	}
}

func TestRunnerConfigRejectsKubernetesTTLOverflow(t *testing.T) {
	config := RunnerConfig{
		Backend:   BackendKubernetesJob,
		Timeout:   time.Minute,
		Artifacts: RunnerArtifactConfig{Directory: "/runner-artifacts", MaxRequestBytes: 1, MaxResultBytes: 1},
		KubernetesJob: KubernetesJobRunnerConfig{
			Image:                   "registry.invalid/thread-keep@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Namespace:               "thread-keep",
			JobServiceAccount:       "runner",
			ArtifactClaim:           "artifacts",
			ArtifactFSGroup:         65532,
			CPULimitMillis:          1,
			MemoryLimitBytes:        1,
			TTLSecondsAfterFinished: math.MaxInt32 + 1,
		},
	}

	if err := ValidateRunnerConfig(config); err == nil {
		t.Fatal("ValidateRunnerConfig() error = nil, want TTL overflow validation error")
	}
}

func TestBuildRunnerProvidesOptInInProcessBackend(t *testing.T) {
	runner, err := BuildRunner(RunnerConfig{Backend: BackendInProcess, Timeout: time.Minute, ReconcileIntervalSeconds: 30})
	if err != nil {
		t.Fatalf("BuildRunner(in_process) error = %v", err)
	}
	if _, ok := runner.(guardedSourceRunner); !ok {
		t.Fatalf("BuildRunner(in_process) type = %T, want guardedSourceRunner", runner)
	}
}

func TestGuardedSourceRunnerConvertsPanicToTypedFailure(t *testing.T) {
	runner := guardedSourceRunner{source: panicSourceRunner{}}
	_, err := runner.IndexSource(context.Background(), planner.SourceRequest{})
	if domain.CodeOf(err) != domain.CodeCoverageIncomplete {
		t.Fatalf("guardedSourceRunner.IndexSource() code = %q, want %q", domain.CodeOf(err), domain.CodeCoverageIncomplete)
	}
	if strings.Contains(err.Error(), "credential-shaped") {
		t.Fatalf("guardedSourceRunner.IndexSource() leaked panic detail: %v", err)
	}
}
