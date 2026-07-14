package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/remote"
	"github.com/zeebo/blake3"
)

const (
	writerToken = "writer-token"
	readerToken = "reader-token"
	flakyToken  = "flaky-token"
)

func newFakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/acme/thread-keep" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.Header.Get("Authorization") {
		case "Bearer " + writerToken:
			_, _ = writer.Write([]byte(`{"permissions":{"push":true,"pull":true}}`))
		case "Bearer " + readerToken:
			_, _ = writer.Write([]byte(`{"permissions":{"push":false,"pull":true}}`))
		case "Bearer " + flakyToken:
			writer.WriteHeader(http.StatusInternalServerError)
		default:
			writer.WriteHeader(http.StatusUnauthorized)
		}
	}))
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	github := newFakeGitHub(t)
	t.Cleanup(github.Close)
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewHandler(store, Config{
		GitHubAPIBaseURL: github.URL,
		Repositories: map[string]RepositoryConfig{
			"repo-1": {GitHubOwner: "acme", GitHubRepo: "thread-keep"},
		},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func testObject(body string) (string, []byte) {
	contents := []byte(body)
	digest := blake3.Sum256(contents)
	return fmt.Sprintf("%x", digest[:]), contents
}

func call(t *testing.T, method, target, token string, body []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do(%s %s) error = %v", method, target, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	return response.StatusCode, payload
}

func errorCode(t *testing.T, payload []byte) string {
	t.Helper()
	var decoded struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.Code == "" || decoded.Message == "" {
		t.Fatalf("error payload %q is not a typed {code,message} object (%v)", payload, err)
	}
	return decoded.Code
}

func refURL(server *httptest.Server, repository string) string {
	return server.URL + "/v1/repositories/" + repository + "/refs/refs%2Fcontexts%2Fmain"
}

func objectURL(server *httptest.Server, repository, id string) string {
	return server.URL + "/v1/repositories/" + repository + "/objects/" + id
}

func TestServerEnforcesGitHubPermissionsPerOperation(t *testing.T) {
	server := newTestServer(t)
	objectID, contents := testObject("permission object")

	if status, payload := call(t, http.MethodGet, refURL(server, "repo-1"), "", nil); status != http.StatusUnauthorized || errorCode(t, payload) != "auth" {
		t.Fatalf("GET ref without token = %d %s, want 401 auth", status, payload)
	}
	if status, payload := call(t, http.MethodGet, refURL(server, "repo-1"), "unknown-token", nil); status != http.StatusForbidden || errorCode(t, payload) != "auth" {
		t.Fatalf("GET ref with unknown token = %d %s, want 403 auth", status, payload)
	}
	if status, _ := call(t, http.MethodGet, refURL(server, "repo-1"), readerToken, nil); status != http.StatusOK {
		t.Fatalf("GET ref with reader token = %d, want 200", status)
	}
	if status, payload := call(t, http.MethodPut, objectURL(server, "repo-1", objectID), readerToken, contents); status != http.StatusForbidden || errorCode(t, payload) != "auth" {
		t.Fatalf("PUT object with reader token = %d %s, want 403 auth", status, payload)
	}
	if status, _ := call(t, http.MethodPut, objectURL(server, "repo-1", objectID), writerToken, contents); status != http.StatusCreated {
		t.Fatalf("PUT object with writer token = %d, want 201", status)
	}
	if status, _ := call(t, http.MethodGet, objectURL(server, "repo-1", objectID), readerToken, nil); status != http.StatusOK {
		t.Fatalf("GET object with reader token = %d, want 200", status)
	}
}

func TestServerVerifiesObjectContentHash(t *testing.T) {
	server := newTestServer(t)
	objectID, contents := testObject("hash-checked object")

	if status, payload := call(t, http.MethodPut, objectURL(server, "repo-1", objectID), writerToken, []byte("tampered")); status != http.StatusBadRequest || errorCode(t, payload) != "validation" {
		t.Fatalf("PUT tampered object = %d %s, want 400 validation", status, payload)
	}
	if status, _ := call(t, http.MethodPut, objectURL(server, "repo-1", objectID), writerToken, contents); status != http.StatusCreated {
		t.Fatalf("PUT object = %d, want 201", status)
	}
	if status, _ := call(t, http.MethodPut, objectURL(server, "repo-1", objectID), writerToken, contents); status != http.StatusOK {
		t.Fatalf("PUT object again = %d, want 200 idempotent", status)
	}
	status, payload := call(t, http.MethodGet, objectURL(server, "repo-1", objectID), readerToken, nil)
	if status != http.StatusOK || !bytes.Equal(payload, contents) {
		t.Fatalf("GET object = %d %q, want stored contents", status, payload)
	}
}

func TestServerCompareAndSwapRefEnforcesVersions(t *testing.T) {
	server := newTestServer(t)
	commitID, contents := testObject("ref target object")
	if status, _ := call(t, http.MethodPut, objectURL(server, "repo-1", commitID), writerToken, contents); status != http.StatusCreated {
		t.Fatalf("PUT object = want 201")
	}

	next := remote.Ref{RefName: "refs/contexts/main", CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 1}
	payload, _ := json.Marshal(map[string]remote.Ref{"expected": {RefName: "refs/contexts/main"}, "next": next})
	if status, body := call(t, http.MethodPut, refURL(server, "repo-1"), writerToken, payload); status != http.StatusOK {
		t.Fatalf("PUT ref CAS = %d %s, want 200", status, body)
	}

	status, body := call(t, http.MethodGet, refURL(server, "repo-1"), readerToken, nil)
	var stored remote.Ref
	if err := json.Unmarshal(body, &stored); status != http.StatusOK || err != nil || stored != next {
		t.Fatalf("GET ref = %d %s, want stored ref %+v", status, body, next)
	}

	if status, body := call(t, http.MethodPut, refURL(server, "repo-1"), writerToken, payload); status != http.StatusConflict || errorCode(t, body) != "remote_conflict" {
		t.Fatalf("PUT stale ref CAS = %d %s, want 409 remote_conflict", status, body)
	}
}

func TestServerControlJSONAllowsTrailingValues(t *testing.T) {
	t.Run("GitHub permissions use the first JSON value", func(t *testing.T) {
		github := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = writer.Write([]byte(`{"permissions":{"push":true,"pull":true}} {"ignored":true}`))
		}))
		defer github.Close()
		store, err := OpenStorage(t.TempDir(), "")
		if err != nil {
			t.Fatalf("OpenStorage() error = %v", err)
		}
		defer store.Close()
		handler, err := NewHandler(store, Config{
			GitHubAPIBaseURL: github.URL,
			Repositories: map[string]RepositoryConfig{
				"repo-1": {GitHubOwner: "acme", GitHubRepo: "thread-keep"},
			},
		})
		if err != nil {
			t.Fatalf("NewHandler() error = %v", err)
		}
		server := httptest.NewServer(handler)
		defer server.Close()

		if status, payload := call(t, http.MethodGet, refURL(server, "repo-1"), writerToken, nil); status != http.StatusOK {
			t.Fatalf("GET ref with trailing GitHub JSON = %d %s, want 200", status, payload)
		}
	})

	t.Run("compare-and-swap uses the first JSON value", func(t *testing.T) {
		server := newTestServer(t)
		commitID, contents := testObject("trailing JSON ref target")
		if status, payload := call(t, http.MethodPut, objectURL(server, "repo-1", commitID), writerToken, contents); status != http.StatusCreated {
			t.Fatalf("PUT object = %d %s, want 201", status, payload)
		}
		next := remote.Ref{RefName: "refs/contexts/main", CommitID: commitID, SourceSHA: "2222222222222222222222222222222222222222", Version: 1}
		payload, err := json.Marshal(map[string]remote.Ref{"expected": {RefName: "refs/contexts/main"}, "next": next})
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		payload = append(payload, []byte(` {"ignored":true}`)...)

		if status, body := call(t, http.MethodPut, refURL(server, "repo-1"), writerToken, payload); status != http.StatusOK {
			t.Fatalf("PUT ref CAS with trailing JSON = %d %s, want 200", status, body)
		}
	})
}

func TestServerReturnsTypedErrorsForUnknownRoutes(t *testing.T) {
	server := newTestServer(t)
	if status, payload := call(t, http.MethodGet, refURL(server, "unknown-repo"), readerToken, nil); status != http.StatusNotFound || errorCode(t, payload) != "validation" {
		t.Fatalf("GET unknown repository = %d %s, want 404 validation", status, payload)
	}
	if status, payload := call(t, http.MethodDelete, refURL(server, "repo-1"), writerToken, nil); status != http.StatusMethodNotAllowed || errorCode(t, payload) != "validation" {
		t.Fatalf("DELETE ref = %d %s, want 405 validation", status, payload)
	}
	if status, payload := call(t, http.MethodGet, server.URL+"/v1/unrelated", readerToken, nil); status != http.StatusNotFound || errorCode(t, payload) != "validation" {
		t.Fatalf("GET unrelated path = %d %s, want 404 validation", status, payload)
	}
	missingID, _ := testObject("never uploaded")
	if status, payload := call(t, http.MethodGet, objectURL(server, "repo-1", missingID), readerToken, nil); status != http.StatusNotFound {
		t.Fatalf("GET missing object = %d %s, want 404", status, payload)
	} else if code := errorCode(t, payload); code != "object_missing" || !strings.Contains(string(payload), "missing") {
		t.Fatalf("GET missing object payload = %s, want object missing", payload)
	}
}

func TestServerDistinguishesGitHubOutageFromDenial(t *testing.T) {
	server := newTestServer(t)
	status, payload := call(t, http.MethodGet, refURL(server, "repo-1"), flakyToken, nil)
	if status != http.StatusBadGateway {
		t.Fatalf("GET ref during GitHub outage = %d %s, want 502", status, payload)
	}
	if code := errorCode(t, payload); code == "auth" {
		t.Fatalf("GitHub outage reported as auth denial: %s", payload)
	}
}

func TestServerRefusesGitHubRedirectWithoutLeakingToken(t *testing.T) {
	const token = "github-redirect-secret-token"
	var secondHopRequests atomic.Int32
	secondHop := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secondHopRequests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"permissions":{"push":true,"pull":true}}`))
	}))
	defer secondHop.Close()
	firstHop := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, secondHop.URL+"/redirected", http.StatusMovedPermanently)
	}))
	defer firstHop.Close()

	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewHandler(store, Config{
		GitHubAPIBaseURL: firstHop.URL,
		Repositories: map[string]RepositoryConfig{
			"repo-1": {GitHubOwner: "acme", GitHubRepo: "thread-keep"},
		},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	status, payload := call(t, http.MethodGet, refURL(server, "repo-1"), token, nil)
	if status != http.StatusBadGateway || errorCode(t, payload) != "local_storage" {
		t.Fatalf("GET ref with GitHub redirect = %d %s, want 502 local_storage", status, payload)
	}
	if got := secondHopRequests.Load(); got != 0 {
		t.Fatalf("GitHub redirect second-hop requests = %d, want 0", got)
	}
	if strings.Contains(string(payload), token) {
		t.Fatalf("GitHub redirect response leaks the token: %s", payload)
	}
}

func TestServerGitHubClientConfiguresTransportTimeoutsWithoutWholeRequestTimeout(t *testing.T) {
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewHandler(store, Config{
		GitHubAPIBaseURL: "https://api.github.invalid",
		Repositories: map[string]RepositoryConfig{
			"repo-1": {GitHubOwner: "acme", GitHubRepo: "thread-keep"},
		},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	client := handler.(*Server).github
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("NewHandler() GitHub transport = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("NewHandler() GitHub transport DialContext is nil, want bounded dialer")
	}
	if got, want := githubDialTimeout, 10*time.Second; got != want {
		t.Fatalf("NewHandler() GitHub dial timeout = %s, want %s", got, want)
	}
	if got, want := transport.ResponseHeaderTimeout, 30*time.Second; got != want {
		t.Fatalf("NewHandler() GitHub ResponseHeaderTimeout = %s, want %s", got, want)
	}
	if client.Timeout != 0 {
		t.Fatalf("NewHandler() GitHub whole-request timeout = %s, want 0", client.Timeout)
	}
}
