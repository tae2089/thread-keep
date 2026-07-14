package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	githubforge "github.com/tae2089/thread-keep/internal/forge/github"
	"github.com/tae2089/thread-keep/internal/remote/server"
)

const githubPrivateKeyFileEnvironment = "THREAD_KEEP_GITHUB_APP_PRIVATE_KEY_FILE"

func buildCoordinator(config server.Config, refs *server.GormRefStore, objects server.Storage) (*server.Coordinator, error) {
	repositories, err := coordinatorRepositories(config)
	if err != nil || len(repositories) == 0 {
		return nil, err
	}
	if config.GitHubApp == nil || config.GitHubApp.AppID < 1 || config.GitHubApp.InstallationID < 1 {
		return nil, errors.New("planning requires github_app.app_id and github_app.installation_id")
	}
	privateKeyFile := strings.TrimSpace(os.Getenv(githubPrivateKeyFileEnvironment))
	if privateKeyFile == "" {
		return nil, errors.New("planning control API requires GitHub App private-key secret configuration")
	}
	privateKey, err := os.ReadFile(privateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read GitHub App private key file: %w", err)
	}
	defer clear(privateKey)
	tokenSource, err := githubforge.NewAppTokenSource(githubforge.AppTokenConfig{APIBaseURL: config.GitHubAPIBaseURL, AppID: strconv.FormatInt(config.GitHubApp.AppID, 10), InstallationID: config.GitHubApp.InstallationID, PrivateKeyPEM: privateKey})
	if err != nil {
		return nil, err
	}
	adapter, err := githubforge.NewAdapter(githubforge.AdapterConfig{APIBaseURL: config.GitHubAPIBaseURL, TokenSource: tokenSource})
	if err != nil {
		return nil, err
	}
	return server.NewCoordinator(server.CoordinatorConfig{Refs: refs, Objects: objects, Forge: adapter, CheckPublisher: adapter, Repositories: repositories})
}

func coordinatorRepositories(config server.Config) ([]server.CoordinatorRepository, error) {
	repositories := make([]server.CoordinatorRepository, 0, len(config.Repositories))
	installationID := int64(1)
	if config.GitHubApp != nil && config.GitHubApp.InstallationID > 0 {
		installationID = config.GitHubApp.InstallationID
	}
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
		repositories = append(repositories, server.CoordinatorRepository{RemoteKey: remoteKey, ContextRepositoryID: repository.ContextRepositoryID, TargetRef: "refs/contexts/" + branch, ForgeRepository: repository.GitHubOwner + "/" + repository.GitHubRepo, InstallationID: installationID, AutomaticLanding: planning.AutomaticLanding, MaxAttempts: maxAttempts})
	}
	return repositories, nil
}
