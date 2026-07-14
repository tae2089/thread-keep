package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/tae2089/thread-keep/internal/remote/server"
)

func TestBuildCoordinatorUsesPrivateKeyWithoutWebhookOrPlannerSecrets(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	privateKeyFile := filepath.Join(t.TempDir(), "github-app.pem")
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(privateKeyFile, privateKey, 0o600); err != nil {
		t.Fatalf("WriteFile(private key) error = %v", err)
	}
	t.Setenv(githubPrivateKeyFileEnvironment, privateKeyFile)
	storage, err := server.OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	config := server.Config{GitHubAPIBaseURL: "https://api.github.com", GitHubApp: &server.GitHubAppConfig{AppID: 123, InstallationID: 456}, Repositories: map[string]server.RepositoryConfig{"repo": {GitHubOwner: "owner", GitHubRepo: "repository", ContextRepositoryID: "context-repo", Planning: &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main"}}}}}
	coordinator, err := buildCoordinator(config, storage.RefStore(), storage)
	if err != nil || coordinator == nil {
		t.Fatalf("buildCoordinator() = %v, %v", coordinator, err)
	}
}

func TestCoordinatorRepositoriesRequiresExplicitCompatibleOptIn(t *testing.T) {
	base := server.RepositoryConfig{GitHubOwner: "owner", GitHubRepo: "repository", ContextRepositoryID: "context-repo"}
	tests := []struct {
		name       string
		planning   *server.PlanningConfig
		wantCount  int
		wantTarget string
		wantError  bool
	}{
		{name: "disabled", planning: &server.PlanningConfig{}, wantCount: 0},
		{name: "preview only", planning: &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main"}, CheckMode: "informational", ContextSchema: 4}, wantCount: 1, wantTarget: "refs/contexts/main"},
		{name: "automatic", planning: &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main"}, AutomaticLanding: true, MaxAttempts: 5}, wantCount: 1, wantTarget: "refs/contexts/main"},
		{name: "multiple branches", planning: &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main", "release"}}, wantError: true},
		{name: "required check", planning: &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main"}, CheckMode: "required"}, wantError: true},
		{name: "legacy schema", planning: &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main"}, ContextSchema: 3}, wantError: true},
		{name: "unbounded retry", planning: &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main"}, MaxAttempts: 11}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository := base
			repository.Planning = test.planning
			result, err := coordinatorRepositories(server.Config{Repositories: map[string]server.RepositoryConfig{"repo": repository}})
			if (err != nil) != test.wantError {
				t.Fatalf("coordinatorRepositories() error = %v, wantError %t", err, test.wantError)
			}
			if err != nil {
				return
			}
			if len(result) != test.wantCount {
				t.Fatalf("coordinatorRepositories() = %+v, want count %d", result, test.wantCount)
			}
			if test.wantCount == 1 && (result[0].TargetRef != test.wantTarget || result[0].ForgeRepository != "owner/repository") {
				t.Fatalf("coordinatorRepositories() = %+v", result)
			}
		})
	}
}
