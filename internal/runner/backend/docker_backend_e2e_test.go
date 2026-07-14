package backend_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/tae2089/thread-keep/internal/runner/backend"
)

const (
	dockerBackendE2EEndpoint = "THREAD_KEEP_DOCKER_BACKEND_E2E_ENDPOINT"
	dockerBackendE2EImage    = "THREAD_KEEP_DOCKER_BACKEND_E2E_IMAGE"
	dockerBackendE2ESHA      = "THREAD_KEEP_DOCKER_BACKEND_E2E_SHA"
)

func TestDockerBackendRunsRealFileProtocol(t *testing.T) {
	endpoint := os.Getenv(dockerBackendE2EEndpoint)
	image := os.Getenv(dockerBackendE2EImage)
	fixtureSHA := os.Getenv(dockerBackendE2ESHA)
	if endpoint == "" || image == "" || fixtureSHA == "" {
		t.Skipf("set %s, %s and %s to run the Docker backend E2E", dockerBackendE2EEndpoint, dockerBackendE2EImage, dockerBackendE2ESHA)
	}
	engine, err := backend.NewMobyDockerEngineFromEndpoint(endpoint)
	if err != nil {
		t.Fatalf("NewMobyDockerEngineFromEndpoint() error = %v", err)
	}
	runnerBackend, err := backend.NewDockerBackend(backend.DockerBackendConfig{
		Engine:                engine,
		Image:                 image,
		Network:               "none",
		CPULimitMillis:        500,
		MemoryLimitBytes:      256 << 20,
		WorkspaceLimitBytes:   256 << 20,
		MaxRequestBytes:       1 << 20,
		MaxResultBytes:        16 << 20,
		CredentialWaitTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewDockerBackend() error = %v", err)
	}
	spec := backend.ExecutionSpec{
		ExecutionID:   strings.Repeat("1", 64),
		AttemptID:     strings.Repeat("2", 64),
		RunnerAttempt: 1,
		RequestDigest: strings.Repeat("3", 64),
		SpecDigest:    strings.Repeat("4", 64),
		Timeout:       90 * time.Second,
		Request: planner.SourceRequest{
			Mode:          planner.SourceFinal,
			RepositoryID:  "docker-e2e-fixture",
			TargetRef:     "refs/contexts/main",
			RepositoryURL: "file:///opt/thread-keep-fixture",
			Credential:    "non-secret-local-fixture-token",
			FinalSHA:      fixtureSHA,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	handle, err := runnerBackend.Ensure(ctx, spec)
	if handle.ResourceID != "" {
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cleanupCancel()
			if err := runnerBackend.Cleanup(cleanupCtx, handle); err != nil {
				t.Errorf("Cleanup() error = %v", err)
			}
		})
	}
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	for {
		observation, err := runnerBackend.Observe(ctx, handle)
		if err != nil {
			t.Fatalf("Observe() error = %v", err)
		}
		switch observation.State {
		case backend.ObservationRunning:
			select {
			case <-ctx.Done():
				t.Fatalf("Docker backend E2E timed out: %v", ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		case backend.ObservationSucceeded:
			envelope, err := backend.DecodeResult(observation.ResultEnvelope)
			if err != nil {
				t.Fatalf("DecodeResult() error = %v", err)
			}
			if envelope.Code != "" || envelope.Evidence.SourceSHA != fixtureSHA || !envelope.Evidence.CoverageComplete {
				t.Fatalf("result envelope = %+v", envelope)
			}
			return
		default:
			t.Fatalf("Docker backend E2E observation = %+v", observation)
		}
	}
}
