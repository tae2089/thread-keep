package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const testClusterSecret = "cluster-secret-value"

func newClusterAuthTestServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var githubCalls atomic.Int64
	github := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		githubCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"permissions":{"push":true,"pull":true}}`))
	}))
	t.Cleanup(github.Close)
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewClusterHandler(store, store, testClusterSecret, Config{
		GitHubAPIBaseURL: github.URL,
		Repositories: map[string]RepositoryConfig{
			"repo-1": {GitHubOwner: "acme", GitHubRepo: "thread-keep"},
		},
	})
	if err != nil {
		t.Fatalf("NewClusterHandler() error = %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, &githubCalls
}

func clusterCall(t *testing.T, method, target, secret string, body []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, target, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if secret != "" {
		request.Header.Set("X-Thread-Keep-Cluster-Secret", secret)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	return response.StatusCode, payload
}

func TestClusterSecretAuthorizesWithoutGitHub(t *testing.T) {
	server, githubCalls := newClusterAuthTestServer(t)
	objectID, contents := testObject("cluster-authored object")

	status, _ := clusterCall(t, http.MethodPut, objectURL(server, "repo-1", objectID), testClusterSecret, contents)
	if status != http.StatusCreated {
		t.Fatalf("cluster PUT object = %d, want 201", status)
	}
	status, payload := clusterCall(t, http.MethodGet, objectURL(server, "repo-1", objectID), testClusterSecret, nil)
	if status != http.StatusOK || string(payload) != string(contents) {
		t.Fatalf("cluster GET object = %d %q, want stored contents", status, payload)
	}
	if githubCalls.Load() != 0 {
		t.Fatalf("cluster principal caused %d GitHub verification calls, want 0", githubCalls.Load())
	}
}

func TestClusterSecretRejectionsAndFallback(t *testing.T) {
	server, githubCalls := newClusterAuthTestServer(t)

	status, payload := clusterCall(t, http.MethodGet, refURL(server, "repo-1"), "wrong-secret", nil)
	if status != http.StatusForbidden || errorCode(t, payload) != "auth" {
		t.Fatalf("wrong cluster secret = %d %s, want 403 auth", status, payload)
	}
	if strings.Contains(string(payload), testClusterSecret) || strings.Contains(string(payload), "wrong-secret") {
		t.Fatalf("cluster auth error leaks a secret value: %s", payload)
	}

	if status, payload = clusterCall(t, http.MethodGet, refURL(server, "repo-1"), "", nil); status != http.StatusUnauthorized || errorCode(t, payload) != "auth" {
		t.Fatalf("no credential = %d %s, want 401 via GitHub path", status, payload)
	}

	if status, _ = call(t, http.MethodGet, refURL(server, "repo-1"), readerToken, nil); status != http.StatusOK {
		t.Fatalf("github bearer on cluster server = %d, want 200", status)
	}
	if githubCalls.Load() == 0 {
		t.Fatalf("github bearer path skipped GitHub verification")
	}
}

func TestSingleNodeServerIgnoresClusterHeader(t *testing.T) {
	server := newTestServer(t)
	status, payload := clusterCall(t, http.MethodGet, refURL(server, "repo-1"), testClusterSecret, nil)
	if status != http.StatusUnauthorized || errorCode(t, payload) != "auth" {
		t.Fatalf("cluster header on single-node server = %d %s, want 401 (header ignored)", status, payload)
	}
}
