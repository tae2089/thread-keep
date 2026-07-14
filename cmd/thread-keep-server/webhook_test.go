package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tae2089/thread-keep/internal/remote/server"
)

const testWebhookSecret = "server-webhook-secret"

func TestBuildPublicHandlerRequiresWebhookSecretWhenPlanningIsEnabled(t *testing.T) {
	storage, err := server.OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	t.Setenv(githubWebhookSecretEnvironment, "")

	_, err = buildPublicHandler(webhookTestConfig(), storage.RefStore(), http.NotFoundHandler())
	if err == nil || !strings.Contains(err.Error(), githubWebhookSecretEnvironment) {
		t.Fatalf("buildPublicHandler() error = %v, want missing webhook secret", err)
	}
}

func TestBuildPublicHandlerDoesNotRequireWebhookSecretWithoutPlanning(t *testing.T) {
	storage, err := server.OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	t.Setenv(githubWebhookSecretEnvironment, "")
	baseCalls := 0
	base := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		baseCalls++
		writer.WriteHeader(http.StatusNoContent)
	})
	config := server.Config{GitHubAPIBaseURL: "https://api.github.invalid", Repositories: map[string]server.RepositoryConfig{"repo": {GitHubOwner: "owner", GitHubRepo: "repository"}}}

	handler, err := buildPublicHandler(config, storage.RefStore(), base)
	if err != nil {
		t.Fatalf("buildPublicHandler() error = %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/repositories/repo/refs/main", nil))
	if response.Code != http.StatusNoContent || baseCalls != 1 {
		t.Fatalf("base response = %d, calls = %d, want 204 and 1", response.Code, baseCalls)
	}
}

func TestBuildPublicHandlerRoutesSignedWebhookAndPreservesBaseRoutes(t *testing.T) {
	storage, err := server.OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	t.Setenv(githubWebhookSecretEnvironment, testWebhookSecret)
	baseCalls := 0
	base := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		baseCalls++
		writer.WriteHeader(http.StatusNoContent)
	})

	handler, err := buildPublicHandler(webhookTestConfig(), storage.RefStore(), base)
	if err != nil {
		t.Fatalf("buildPublicHandler() error = %v", err)
	}
	baseResponse := httptest.NewRecorder()
	handler.ServeHTTP(baseResponse, httptest.NewRequest(http.MethodGet, "/v1/repositories/repo/capabilities", nil))
	if baseResponse.Code != http.StatusNoContent || baseCalls != 1 {
		t.Fatalf("base response = %d, calls = %d, want 204 and 1", baseResponse.Code, baseCalls)
	}

	body := []byte(`{"action":"opened","number":42,"repository":{"full_name":"owner/repository"},"pull_request":{"base":{"ref":"main","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"head":{"ref":"feature","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}},"installation":{"id":456}}`)
	request := signedWebhookRequest(body, "delivery-server-owned", testWebhookSecret)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("webhook response = %d %s, want 202", response.Code, response.Body.String())
	}
	if baseCalls != 1 {
		t.Fatalf("base calls after webhook = %d, want 1", baseCalls)
	}
	if _, err := storage.RefStore().WebhookEvent(request.Context(), "github", "delivery-server-owned"); err != nil {
		t.Fatalf("WebhookEvent() error = %v", err)
	}

	duplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateResponse, signedWebhookRequest(body, "delivery-server-owned", testWebhookSecret))
	var duplicate server.WebhookIntakeResult
	if err := json.Unmarshal(duplicateResponse.Body.Bytes(), &duplicate); err != nil {
		t.Fatalf("decode duplicate response: %v", err)
	}
	if duplicateResponse.Code != http.StatusAccepted || !duplicate.Duplicate {
		t.Fatalf("duplicate response = %d %+v, want 202 duplicate", duplicateResponse.Code, duplicate)
	}
}

func TestBuildPublicHandlerRejectsInvalidWebhookWithoutFallingBack(t *testing.T) {
	storage, err := server.OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	t.Setenv(githubWebhookSecretEnvironment, testWebhookSecret)
	baseCalls := 0
	base := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		baseCalls++
		writer.WriteHeader(http.StatusNoContent)
	})
	handler, err := buildPublicHandler(webhookTestConfig(), storage.RefStore(), base)
	if err != nil {
		t.Fatalf("buildPublicHandler() error = %v", err)
	}

	request := signedWebhookRequest([]byte(`{"event":"invalid"}`), "delivery-invalid", "wrong-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || baseCalls != 0 {
		t.Fatalf("invalid webhook response = %d, base calls = %d, want 403 and 0", response.Code, baseCalls)
	}
}

func webhookTestConfig() server.Config {
	return server.Config{
		GitHubAPIBaseURL: "https://api.github.invalid",
		GitHubApp:        &server.GitHubAppConfig{AppID: 123, InstallationID: 456},
		Repositories: map[string]server.RepositoryConfig{
			"repo": {
				GitHubOwner:         "owner",
				GitHubRepo:          "repository",
				ContextRepositoryID: "context-repo",
				Planning:            &server.PlanningConfig{Enabled: true, TargetBranches: []string{"main"}},
			},
		},
	}
}

func signedWebhookRequest(body []byte, deliveryID, secret string) *http.Request {
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write(body)
	request := httptest.NewRequest(http.MethodPost, server.GitHubWebhookPath, strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Delivery", deliveryID)
	request.Header.Set("X-GitHub-Event", "pull_request")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(digest.Sum(nil)))
	return request
}
