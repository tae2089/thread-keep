package app

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/remote/server"
)

func TestDeployableBuildersKeepWebhookAndRunnerSecretsSeparate(t *testing.T) {
	config := server.Config{GitHubAPIBaseURL: "https://api.github.invalid", GitHubApp: &server.GitHubAppConfig{AppID: 123, InstallationID: 7}, Repositories: map[string]server.RepositoryConfig{"repo": {GitHubOwner: "owner", GitHubRepo: "repository", ContextRepositoryID: "context-repo", Planning: &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main"}}}}}
	refs, err := server.OpenGormRefStore(t.TempDir() + "/ingress.db")
	if err != nil {
		t.Fatalf("OpenGormRefStore() error = %v", err)
	}
	t.Cleanup(func() { _ = refs.Close() })
	if _, err := BuildWebhookIngress(config, refs, []byte("webhook-only-secret")); err != nil {
		t.Fatalf("BuildWebhookIngress() error = %v", err)
	}
	objects, err := server.OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey() error = %v", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	runnerConfig, err := ResolveRunnerConfig(RunnerConfig{}, RunnerDefaults{ProcessPath: executable, Timeout: 2 * time.Minute}, RunnerOverrides{})
	if err != nil {
		t.Fatalf("ResolveRunnerConfig() error = %v", err)
	}
	if _, err := BuildCoordinator(config, objects.RefStore(), objects, runnerConfig, privatePEM); err != nil {
		t.Fatalf("BuildCoordinator() without webhook secret error = %v", err)
	}
}

func TestNewProcessRunnerPreservesConfiguredTimeout(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	timeout := 37 * time.Second
	runner, err := newProcessRunner(executable, timeout)
	if err != nil {
		t.Fatalf("newProcessRunner() error = %v", err)
	}
	if runner.Timeout != timeout {
		t.Fatalf("newProcessRunner().Timeout = %s, want %s", runner.Timeout, timeout)
	}
}

func TestBuildCoordinatorAssemblesDockerBackendWithoutDialing(t *testing.T) {
	config := server.Config{GitHubAPIBaseURL: "https://api.github.invalid", GitHubApp: &server.GitHubAppConfig{AppID: 123, InstallationID: 7}, Repositories: map[string]server.RepositoryConfig{"repo": {GitHubOwner: "owner", GitHubRepo: "repository", ContextRepositoryID: "context-repo", Planning: &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main"}}}}}
	objects, err := server.OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey() error = %v", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	runnerConfig, err := ResolveRunnerConfig(RunnerConfig{
		Backend: BackendDocker,
		Docker: DockerRunnerConfig{
			Endpoint:            "unix:///path-that-is-not-dialed-during-assembly/docker.sock",
			Image:               "registry.invalid/thread-keep-runner@sha256:" + strings.Repeat("a", 64),
			Network:             "none",
			CPULimitMillis:      250,
			MemoryLimitBytes:    64 << 20,
			WorkspaceLimitBytes: 256 << 20,
			CleanupTTLSeconds:   60,
		},
	}, RunnerDefaults{ProcessPath: "thread-keep-runner", Timeout: 2 * time.Minute}, RunnerOverrides{})
	if err != nil {
		t.Fatalf("ResolveRunnerConfig() error = %v", err)
	}
	if _, err := BuildCoordinator(config, objects.RefStore(), objects, runnerConfig, privatePEM); err != nil {
		t.Fatalf("BuildCoordinator(docker) error = %v", err)
	}
}
