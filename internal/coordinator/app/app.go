package app

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"

	githubforge "github.com/tae2089/thread-keep/internal/forge/github"
	"github.com/tae2089/thread-keep/internal/remote/server"
	"github.com/tae2089/thread-keep/internal/runner/backend"
)

func Repositories(config server.Config) ([]server.CoordinatorRepository, error) {
	if config.GitHubApp == nil || config.GitHubApp.InstallationID < 1 {
		return nil, errors.New("planning requires github_app.installation_id")
	}
	repositories := make([]server.CoordinatorRepository, 0, len(config.Repositories))
	for remoteKey, repository := range config.Repositories {
		planning := repository.Planning
		if planning == nil || !planning.Enabled {
			continue
		}
		if repository.ContextRepositoryID == "" || len(planning.TargetBranches) != 1 || strings.TrimSpace(planning.TargetBranches[0]) == "" {
			return nil, fmt.Errorf("repository %q planning requires context_repository_id and exactly one target branch", remoteKey)
		}
		if planning.CheckMode != "" && planning.CheckMode != "informational" {
			return nil, fmt.Errorf("repository %q planning supports only informational checks", remoteKey)
		}
		if planning.ContextSchema != 0 && planning.ContextSchema != 4 {
			return nil, fmt.Errorf("repository %q planning requires context schema 4", remoteKey)
		}
		maxAttempts := planning.MaxAttempts
		if maxAttempts == 0 {
			maxAttempts = 3
		}
		if maxAttempts < 1 || maxAttempts > 10 {
			return nil, fmt.Errorf("repository %q planning max_attempts must be between 1 and 10", remoteKey)
		}
		branch := strings.TrimSpace(planning.TargetBranches[0])
		repositories = append(repositories, server.CoordinatorRepository{RemoteKey: remoteKey, ContextRepositoryID: repository.ContextRepositoryID, TargetRef: "refs/contexts/" + branch, ForgeRepository: repository.GitHubOwner + "/" + repository.GitHubRepo, InstallationID: config.GitHubApp.InstallationID, AutomaticLanding: planning.AutomaticLanding, MaxAttempts: maxAttempts})
	}
	return repositories, nil
}

func BuildWebhookIngress(config server.Config, refs *server.GormRefStore, webhookSecret []byte) (*server.WebhookIngress, error) {
	repositories, err := Repositories(config)
	if err != nil {
		return nil, err
	}
	adapter, err := githubforge.NewAdapter(githubforge.AdapterConfig{APIBaseURL: config.GitHubAPIBaseURL, WebhookSecret: webhookSecret})
	if err != nil {
		return nil, err
	}
	return server.NewWebhookIngress(server.WebhookIngressConfig{Refs: refs, Verifier: adapter, Repositories: repositories})
}

func BuildCoordinator(config server.Config, refs *server.GormRefStore, objects server.Storage, runnerConfig RunnerConfig, privateKey []byte) (*server.Coordinator, error) {
	repositories, err := Repositories(config)
	if err != nil {
		return nil, err
	}
	if config.GitHubApp == nil || config.GitHubApp.AppID < 1 || config.GitHubApp.InstallationID < 1 {
		return nil, errors.New("coordinator requires github_app.app_id and github_app.installation_id")
	}
	runnerBackend, runner, err := BuildRunnerBackend(runnerConfig)
	if err != nil {
		return nil, err
	}
	specDigest := RunnerSpecDigest(runnerConfig)
	claimedRunner, err := backend.NewDurableSourceRunner(backend.DurableSourceRunnerConfig{Store: refs, Backend: runnerBackend, InstanceID: rand.Text(), SpecDigest: specDigest, Timeout: runnerConfig.Timeout, CleanupDelay: RunnerCleanupDelay(runnerConfig)})
	if err != nil {
		return nil, err
	}
	tokenSource, err := githubforge.NewAppTokenSource(githubforge.AppTokenConfig{APIBaseURL: config.GitHubAPIBaseURL, AppID: strconv.FormatInt(config.GitHubApp.AppID, 10), InstallationID: config.GitHubApp.InstallationID, PrivateKeyPEM: privateKey})
	if err != nil {
		return nil, err
	}
	adapter, err := githubforge.NewAdapter(githubforge.AdapterConfig{APIBaseURL: config.GitHubAPIBaseURL, TokenSource: tokenSource})
	if err != nil {
		return nil, err
	}
	return server.NewCoordinator(server.CoordinatorConfig{Refs: refs, Objects: objects, Forge: adapter, CheckPublisher: adapter, Runner: runner, ClaimedRunner: claimedRunner, Repositories: repositories})
}
