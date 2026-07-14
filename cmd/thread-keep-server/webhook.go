package main

import (
	"errors"
	"net/http"
	"os"

	"github.com/tae2089/thread-keep/internal/coordinator/app"
	"github.com/tae2089/thread-keep/internal/remote/server"
)

const githubWebhookSecretEnvironment = "THREAD_KEEP_GITHUB_WEBHOOK_SECRET"

func buildPublicHandler(config server.Config, refs *server.GormRefStore, base http.Handler) (http.Handler, error) {
	if base == nil {
		return nil, errors.New("server public handler requires a base handler")
	}
	repositories, err := coordinatorRepositories(config)
	if err != nil {
		return nil, err
	}
	if len(repositories) == 0 {
		return base, nil
	}
	if refs == nil {
		return nil, errors.New("server webhook ingress requires a ref store")
	}
	secret := os.Getenv(githubWebhookSecretEnvironment)
	if secret == "" {
		return nil, errors.New("planning-enabled server requires THREAD_KEEP_GITHUB_WEBHOOK_SECRET")
	}
	ingress, err := app.BuildWebhookIngress(config, refs, []byte(secret))
	if err != nil {
		return nil, err
	}
	webhook, err := server.NewWebhookHandler(ingress)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(server.GitHubWebhookPath, webhook)
	mux.Handle("/", base)
	return mux, nil
}
