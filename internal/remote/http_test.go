package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/zeebo/blake3"
)

const testRefName = "refs/contexts/main"

func testObject(body string) (string, []byte) {
	contents := []byte(body)
	digest := blake3.Sum256(contents)
	return fmt.Sprintf("%x", digest[:]), contents
}

func TestHTTPReadRefParsesRefAndSendsBearerToken(t *testing.T) {
	commitID, _ := testObject("shared context object")
	var gotPath, gotAuthorization, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.EscapedPath()
		gotAuthorization = request.Header.Get("Authorization")
		gotMethod = request.Method
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(Ref{RefName: testRefName, CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 3})
	}))
	defer server.Close()
	transport := NewHTTP(server.URL+"/v1/repositories/repo-1", "secret-token-value")
	ref, err := transport.ReadRef(context.Background(), testRefName)
	if err != nil {
		t.Fatalf("ReadRef() error = %v", err)
	}
	if ref.CommitID != commitID || ref.Version != 3 || ref.RefName != testRefName {
		t.Fatalf("ReadRef() = %+v, want commit %s version 3", ref, commitID)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("ReadRef() method = %s, want GET", gotMethod)
	}
	if want := "/v1/repositories/repo-1/refs/refs%2Fcontexts%2Fmain"; gotPath != want {
		t.Fatalf("ReadRef() path = %s, want %s", gotPath, want)
	}
	if gotAuthorization != "Bearer secret-token-value" {
		t.Fatalf("ReadRef() authorization header missing bearer token")
	}
}

func TestHTTPReadRefReturnsZeroVersionRefWhenAbsent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(Ref{RefName: testRefName, Version: 0})
	}))
	defer server.Close()
	transport := NewHTTP(server.URL, "")
	ref, err := transport.ReadRef(context.Background(), testRefName)
	if err != nil || ref.CommitID != "" || ref.Version != 0 || ref.RefName != testRefName {
		t.Fatalf("ReadRef(absent) = %+v, %v, want zero-version ref", ref, err)
	}
}

func TestHTTPPublishObjectDistinguishesCreatedAndMapsAuthErrors(t *testing.T) {
	objectID, contents := testObject("published object")
	status := http.StatusCreated
	var gotContentType string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotContentType = request.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(request.Body)
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(status)
			_ = json.NewEncoder(writer).Encode(map[string]string{"code": "auth", "message": "token is not authorized for this repository"})
			return
		}
		writer.WriteHeader(status)
	}))
	defer server.Close()
	transport := NewHTTP(server.URL, "secret-token-value")

	created, err := transport.PublishObject(context.Background(), objectID, contents)
	if err != nil || !created {
		t.Fatalf("PublishObject(created) = %v, %v, want true, nil", created, err)
	}
	if gotContentType != "application/octet-stream" || string(gotBody) != string(contents) {
		t.Fatalf("PublishObject() sent content-type %q body %q", gotContentType, gotBody)
	}

	status = http.StatusOK
	created, err = transport.PublishObject(context.Background(), objectID, contents)
	if err != nil || created {
		t.Fatalf("PublishObject(exists) = %v, %v, want false, nil", created, err)
	}

	status = http.StatusForbidden
	_, err = transport.PublishObject(context.Background(), objectID, contents)
	if domain.CodeOf(err) != domain.CodeAuth {
		t.Fatalf("PublishObject(forbidden) error = %v, want %q", err, domain.CodeAuth)
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("PublishObject(forbidden) error leaks the token: %v", err)
	}

	if _, err := transport.PublishObject(context.Background(), objectID, []byte("tampered")); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("PublishObject(tampered) error = %v, want client-side hash validation", err)
	}
}

func TestHTTPReadObjectValidatesContentsAndMapsMissing(t *testing.T) {
	objectID, contents := testObject("fetched object")
	missing := false
	corrupt := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if missing {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(writer).Encode(map[string]string{"code": "object_missing", "message": "remote object " + objectID + " is missing"})
			return
		}
		if corrupt {
			_, _ = writer.Write([]byte("corrupted bytes"))
			return
		}
		_, _ = writer.Write(contents)
	}))
	defer server.Close()
	transport := NewHTTP(server.URL, "")

	got, err := transport.ReadObject(context.Background(), objectID)
	if err != nil || string(got) != string(contents) {
		t.Fatalf("ReadObject() = %q, %v, want stored contents", got, err)
	}

	missing = true
	if _, err := transport.ReadObject(context.Background(), objectID); domain.CodeOf(err) != domain.CodeObjectMissing || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("ReadObject(missing) error = %v, want object missing", err)
	}

	missing = false
	corrupt = true
	if _, err := transport.ReadObject(context.Background(), objectID); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ReadObject(corrupt) error = %v, want content hash validation", err)
	}
}

func TestHTTPCompareAndSwapRefMapsConflictAndSuccess(t *testing.T) {
	commitID, _ := testObject("cas object")
	conflict := false
	var gotMethod, gotPath string
	var gotRequest struct {
		Expected Ref `json:"expected"`
		Next     Ref `json:"next"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotMethod = request.Method
		gotPath = request.URL.EscapedPath()
		_ = json.NewDecoder(request.Body).Decode(&gotRequest)
		writer.Header().Set("Content-Type", "application/json")
		if conflict {
			writer.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(writer).Encode(map[string]string{"code": "remote_conflict", "message": "remote ref changed before compare-and-swap"})
			return
		}
		_ = json.NewEncoder(writer).Encode(gotRequest.Next)
	}))
	defer server.Close()
	transport := NewHTTP(server.URL, "")

	expected := Ref{RefName: testRefName}
	next := Ref{RefName: testRefName, CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 1}
	confirmed, err := transport.CompareAndSwapRef(context.Background(), testRefName, expected, next)
	if err != nil || confirmed != next {
		t.Fatalf("CompareAndSwapRef() = %+v, %v, want confirmed next ref", confirmed, err)
	}
	if gotMethod != http.MethodPut || gotPath != "/refs/refs%2Fcontexts%2Fmain" {
		t.Fatalf("CompareAndSwapRef() request = %s %s, want PUT escaped ref path", gotMethod, gotPath)
	}
	if gotRequest.Expected != expected || gotRequest.Next != next {
		t.Fatalf("CompareAndSwapRef() payload = %+v, want expected and next refs", gotRequest)
	}

	conflict = true
	if _, err := transport.CompareAndSwapRef(context.Background(), testRefName, expected, next); domain.CodeOf(err) != domain.CodeRemoteConflict {
		t.Fatalf("CompareAndSwapRef(conflict) error = %v, want %q", err, domain.CodeRemoteConflict)
	}

	invalid := Ref{RefName: "other", CommitID: commitID, SourceSHA: "1111111111111111111111111111111111111111", Version: 1}
	if _, err := transport.CompareAndSwapRef(context.Background(), testRefName, expected, invalid); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("CompareAndSwapRef(invalid next) error = %v, want %q", err, domain.CodeValidation)
	}
}

func TestHTTPMapsMalformedErrorBodiesToStorageError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html")
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte("<html>gateway exploded</html>"))
	}))
	defer server.Close()
	transport := NewHTTP(server.URL, "secret-token-value")
	_, err := transport.ReadRef(context.Background(), testRefName)
	if domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("ReadRef(malformed error) = %v, want %q", err, domain.CodeLocalStorage)
	}
	if strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestHTTPRefusesRedirectWithoutLeakingToken(t *testing.T) {
	const token = "redirect-secret-token"
	var secondHopRequests atomic.Int32
	secondHop := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secondHopRequests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(Ref{RefName: testRefName, Version: 0})
	}))
	defer secondHop.Close()
	firstHop := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, secondHop.URL+"/redirected", http.StatusMovedPermanently)
	}))
	defer firstHop.Close()

	transport := NewHTTP(firstHop.URL, token)
	_, err := transport.ReadRef(context.Background(), testRefName)
	if domain.CodeOf(err) != domain.CodeLocalStorage || !strings.Contains(err.Error(), "remote redirects are not followed") {
		t.Fatalf("ReadRef(redirect) error = %v, want typed redirect refusal", err)
	}
	if got := secondHopRequests.Load(); got != 0 {
		t.Fatalf("ReadRef(redirect) second-hop requests = %d, want 0", got)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("ReadRef(redirect) error leaks the token: %v", err)
	}
}

func TestHTTPClientConfiguresTransportTimeoutsWithoutWholeRequestTimeout(t *testing.T) {
	client := NewHTTP("https://example.invalid", "").client
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("NewHTTP() transport = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("NewHTTP() transport DialContext is nil, want bounded dialer")
	}
	if got, want := remoteDialTimeout, 10*time.Second; got != want {
		t.Fatalf("NewHTTP() dial timeout = %s, want %s", got, want)
	}
	if got, want := transport.ResponseHeaderTimeout, 30*time.Second; got != want {
		t.Fatalf("NewHTTP() ResponseHeaderTimeout = %s, want %s", got, want)
	}
	if client.Timeout != 0 {
		t.Fatalf("NewHTTP() whole-request timeout = %s, want 0", client.Timeout)
	}
}
