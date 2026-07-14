package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

type staticPeerProvider struct {
	self  Peer
	peers []Peer
	err   error
}

type clusterNode struct {
	server *httptest.Server
	store  *CompositeStorage
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (s *staticPeerProvider) Self() Peer { return s.self }

func (s *staticPeerProvider) Peers(_ context.Context) ([]Peer, error) {
	return append([]Peer(nil), s.peers...), s.err
}

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newPermissiveGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	github := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"permissions":{"push":true,"pull":true}}`))
	}))
	t.Cleanup(github.Close)
	return github
}

func newClusterNode(t *testing.T, github *httptest.Server, peers []Peer, replicationFactor int) clusterNode {
	t.Helper()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	provider := &staticPeerProvider{self: Peer{NodeID: "self", BaseURL: "unused"}, peers: peers}
	replicated, err := NewReplicatedStorage(store, provider, testClusterSecret, replicationFactor)
	if err != nil {
		t.Fatalf("NewReplicatedStorage() error = %v", err)
	}
	handler, err := NewClusterHandler(store, replicated, testClusterSecret, Config{
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
	return clusterNode{server: server, store: store}
}

func TestPeerRequestTimeoutTiers(t *testing.T) {
	tests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{name: "control", got: peerControlTimeout, want: 15 * time.Second},
		{name: "download", got: peerDownloadTimeout, want: 15 * time.Minute},
		{name: "empty upload", got: peerUploadTimeout(0), want: 60 * time.Second},
		{name: "half MiB upload", got: peerUploadTimeout(512 << 10), want: 61 * time.Second},
		{name: "one MiB upload", got: peerUploadTimeout(1 << 20), want: 62 * time.Second},
		{name: "large upload is capped", got: peerUploadTimeout(421 << 20), want: 15 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("timeout = %v, want %v", test.got, test.want)
			}
		})
	}
}

func TestPeerControlRequestUsesShortDeadline(t *testing.T) {
	var remaining time.Duration
	client := newPeerClient(testClusterSecret)
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			t.Fatal("control request context has no deadline")
		}
		remaining = time.Until(deadline)
		return nil, context.DeadlineExceeded
	})

	if _, err := client.listObjects(t.Context(), "http://peer", "repo-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listObjects() error = %v, want context deadline exceeded", err)
	}
	if remaining <= peerControlTimeout-time.Second || remaining > peerControlTimeout {
		t.Fatalf("control deadline remaining = %v, want approximately %v", remaining, peerControlTimeout)
	}
}

func TestPeerObjectListRejectsTrailingJSON(t *testing.T) {
	client := newPeerClient(testClusterSecret)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`["first"] ["second"]`)),
			Header:     make(http.Header),
		}, nil
	})

	if _, err := client.listObjects(t.Context(), "http://peer", "repo-1"); err == nil {
		t.Fatal("listObjects() error = nil, want trailing JSON rejection")
	}
}

func TestPeerObjectReadUsesTransferDeadlineAndCompletesSlowBody(t *testing.T) {
	objectContents := []byte("slow object body")
	peer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(40 * time.Millisecond)
		_, _ = writer.Write(objectContents)
	}))
	t.Cleanup(peer.Close)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	got, err := newPeerClient(testClusterSecret).readObject(ctx, peer.URL, "repo-1", "object-1")
	if err != nil || string(got) != string(objectContents) {
		t.Fatalf("readObject(slow body) = %q, %v, want successful transfer", got, err)
	}

	client := newPeerClient(testClusterSecret)
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			t.Fatal("object request context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= peerControlTimeout || remaining > peerDownloadTimeout {
			t.Fatalf("object deadline remaining = %v, want above %v and at most %v", remaining, peerControlTimeout, peerDownloadTimeout)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("object body")),
			Header:     make(http.Header),
		}, nil
	})
	if _, err := client.readObject(t.Context(), "http://peer", "repo-1", "object-1"); err != nil {
		t.Fatalf("readObject() error = %v", err)
	}
}

func TestPeerObjectPublishUsesSizeScaledTransferDeadline(t *testing.T) {
	contents := make([]byte, 1<<20)
	client := newPeerClient(testClusterSecret)
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			t.Fatal("object publish context has no deadline")
		}
		remaining := time.Until(deadline)
		want := peerUploadTimeout(len(contents))
		if remaining <= want-time.Second || remaining > want {
			t.Fatalf("object publish deadline remaining = %v, want approximately %v", remaining, want)
		}
		got, err := io.ReadAll(request.Body)
		if err != nil || len(got) != len(contents) {
			t.Fatalf("object publish body = %d bytes, %v, want %d bytes", len(got), err, len(contents))
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
		}, nil
	})

	if err := client.publishObject(t.Context(), "http://peer", "repo-1", "object-1", contents); err != nil {
		t.Fatalf("publishObject() error = %v", err)
	}
}

func TestFetchOnMissServesObjectHeldByPeer(t *testing.T) {
	github := newPermissiveGitHub(t)
	nodeB := newClusterNode(t, github, nil, 1)
	nodeA := newClusterNode(t, github, []Peer{{NodeID: "node-b", BaseURL: nodeB.server.URL}}, 1)
	objectID, contents := testObject("only on node b")

	if status, _ := clusterCall(t, http.MethodPut, objectURL(nodeB.server, "repo-1", objectID), testClusterSecret, contents); status != http.StatusCreated {
		t.Fatalf("seed node-b = want 201")
	}

	status, payload := call(t, http.MethodGet, objectURL(nodeA.server, "repo-1", objectID), readerToken, nil)
	if status != http.StatusOK || string(payload) != string(contents) {
		t.Fatalf("GET via node-a (fetch-on-miss) = %d %q, want peer contents", status, payload)
	}

	nodeB.server.Close()
	status, payload = call(t, http.MethodGet, objectURL(nodeA.server, "repo-1", objectID), readerToken, nil)
	if status != http.StatusOK || string(payload) != string(contents) {
		t.Fatalf("GET via node-a after peer death = %d %q, want locally repaired copy", status, payload)
	}
}

func TestFetchOnMissRejectsCorruptPeerBytesAndStopsRecursion(t *testing.T) {
	github := newPermissiveGitHub(t)
	corruptPeer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("corrupted bytes"))
	}))
	t.Cleanup(corruptPeer.Close)
	holder := newClusterNode(t, github, nil, 1)
	nodeA := newClusterNode(t, github, []Peer{{NodeID: "corrupt", BaseURL: corruptPeer.URL}}, 1)
	objectID, contents := testObject("guarded object")

	if status, payload := call(t, http.MethodGet, objectURL(nodeA.server, "repo-1", objectID), readerToken, nil); status != http.StatusNotFound {
		t.Fatalf("GET with corrupt peer = %d %s, want 404 (corrupt bytes rejected)", status, payload)
	}

	if status, _ := clusterCall(t, http.MethodPut, objectURL(holder.server, "repo-1", objectID), testClusterSecret, contents); status != http.StatusCreated {
		t.Fatalf("seed holder = want 201")
	}
	nodeBWithHolder := newClusterNode(t, github, []Peer{{NodeID: "holder", BaseURL: holder.server.URL}}, 1)
	if status, _ := clusterCall(t, http.MethodGet, objectURL(nodeBWithHolder.server, "repo-1", objectID), testClusterSecret, nil); status != http.StatusNotFound {
		t.Fatalf("internal GET = %d, want 404 local-only (no recursive fan-out)", status)
	}
}

func TestFetchOnMissDoesNotHidePackDictionaryCorruption(t *testing.T) {
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	targetID, targetContents := testObject("target")
	dictionaryID, dictionaryContents := testObject("dictionary contents are deliberately longer than target")
	packsDirectory := store.objects.packsDirectory("repo-1")
	packName, err := writePack(packsDirectory, map[string][]byte{
		targetID:     targetContents,
		dictionaryID: dictionaryContents,
	})
	if err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	indexes, err := loadPackIndexes(packsDirectory)
	if err != nil || len(indexes) != 1 {
		t.Fatalf("loadPackIndexes() = %+v, %v, want one index", indexes, err)
	}
	delete(indexes[0].Objects, indexes[0].Dictionary)
	indexContents, err := json.Marshal(indexes[0])
	if err != nil {
		t.Fatalf("Marshal(index) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(packsDirectory, packName+".idx.json"), indexContents, 0o644); err != nil {
		t.Fatalf("WriteFile(corrupt index) error = %v", err)
	}

	var peerRequests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		peerRequests.Add(1)
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(peer.Close)
	replicated, err := NewReplicatedStorage(store, &staticPeerProvider{
		self:  Peer{NodeID: "self", BaseURL: "unused"},
		peers: []Peer{{NodeID: "peer", BaseURL: peer.URL}},
	}, testClusterSecret, 1)
	if err != nil {
		t.Fatalf("NewReplicatedStorage() error = %v", err)
	}

	if _, err := replicated.ReadObject(t.Context(), "repo-1", targetID); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ReadObject(corrupt pack) error = %v, want %q", err, domain.CodeValidation)
	} else if isMissingObjectError(err) {
		t.Fatalf("ReadObject(corrupt pack) error = %v, must not be missing", err)
	}
	if got := peerRequests.Load(); got != 0 {
		t.Fatalf("peer requests = %d, want 0 for local corruption", got)
	}
}

func TestWriteThroughReplicatesToPeersWithQuorum(t *testing.T) {
	github := newPermissiveGitHub(t)
	nodeB := newClusterNode(t, github, nil, 1)
	nodeA := newClusterNode(t, github, []Peer{{NodeID: "node-b", BaseURL: nodeB.server.URL}}, 2)
	objectID, contents := testObject("replicated on write")

	if status, _ := call(t, http.MethodPut, objectURL(nodeA.server, "repo-1", objectID), writerToken, contents); status != http.StatusCreated {
		t.Fatalf("external PUT via node-a = want 201")
	}
	status, payload := clusterCall(t, http.MethodGet, objectURL(nodeB.server, "repo-1", objectID), testClusterSecret, nil)
	if status != http.StatusOK || string(payload) != string(contents) {
		t.Fatalf("node-b copy after write-through = %d %q, want replica", status, payload)
	}
}

func TestWriteThroughQuorumRules(t *testing.T) {
	github := newPermissiveGitHub(t)
	soloNode := newClusterNode(t, github, nil, 2)
	objectID, contents := testObject("solo write")
	if status, _ := call(t, http.MethodPut, objectURL(soloNode.server, "repo-1", objectID), writerToken, contents); status != http.StatusCreated {
		t.Fatalf("solo PUT with factor 2 = want 201 (required shrinks to live nodes)")
	}

	deadPeer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	deadPeer.Close()
	degradedNode := newClusterNode(t, github, []Peer{{NodeID: "dead", BaseURL: deadPeer.URL}}, 2)
	failedID, failedContents := testObject("degraded write")
	status, payload := call(t, http.MethodPut, objectURL(degradedNode.server, "repo-1", failedID), writerToken, failedContents)
	if status != http.StatusInternalServerError || errorCode(t, payload) != "local_storage" {
		t.Fatalf("PUT with dead peer = %d %s, want quorum failure", status, payload)
	}
	if status, _ = clusterCall(t, http.MethodGet, objectURL(degradedNode.server, "repo-1", failedID), testClusterSecret, nil); status != http.StatusOK {
		t.Fatalf("local copy after quorum failure = %d, want 200 (retained for retry/anti-entropy)", status)
	}
}

func TestPublishObjectDistinguishesMembershipErrorFromEmptyView(t *testing.T) {
	t.Run("membership error retains local copy and returns retryable storage error", func(t *testing.T) {
		store, err := OpenStorage(t.TempDir(), "")
		if err != nil {
			t.Fatalf("OpenStorage() error = %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		replicated, err := NewReplicatedStorage(store, &staticPeerProvider{
			self: Peer{NodeID: "self", BaseURL: "unused"},
			err:  errors.New("membership backend unavailable"),
		}, testClusterSecret, 2)
		if err != nil {
			t.Fatalf("NewReplicatedStorage() error = %v", err)
		}
		objectID, contents := testObject("retained after membership failure")

		created, err := replicated.PublishObject(t.Context(), "repo-1", objectID, contents)
		if created || domain.CodeOf(err) != domain.CodeLocalStorage {
			t.Fatalf("PublishObject() = %t, %v, want false and %q", created, err, domain.CodeLocalStorage)
		}
		for _, message := range []string{"cluster membership is unavailable", "local copy retained", "retry is idempotent"} {
			if !strings.Contains(err.Error(), message) {
				t.Fatalf("PublishObject() error = %q, want %q", err, message)
			}
		}
		stored, readErr := store.ReadObject(t.Context(), "repo-1", objectID)
		if readErr != nil || string(stored) != string(contents) {
			t.Fatalf("ReadObject(local copy) = %q, %v, want retained contents", stored, readErr)
		}
	})

	t.Run("empty membership view remains successful", func(t *testing.T) {
		store, err := OpenStorage(t.TempDir(), "")
		if err != nil {
			t.Fatalf("OpenStorage() error = %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		replicated, err := NewReplicatedStorage(store, &staticPeerProvider{
			self: Peer{NodeID: "self", BaseURL: "unused"},
		}, testClusterSecret, 2)
		if err != nil {
			t.Fatalf("NewReplicatedStorage() error = %v", err)
		}
		objectID, contents := testObject("successful empty membership view")

		created, err := replicated.PublishObject(t.Context(), "repo-1", objectID, contents)
		if !created || err != nil {
			t.Fatalf("PublishObject() = %t, %v, want true, nil", created, err)
		}
	})
}

func TestObjectListEndpointIsClusterOnly(t *testing.T) {
	github := newPermissiveGitHub(t)
	node := newClusterNode(t, github, nil, 1)
	objectID, contents := testObject("listable object")
	if status, _ := clusterCall(t, http.MethodPut, objectURL(node.server, "repo-1", objectID), testClusterSecret, contents); status != http.StatusCreated {
		t.Fatalf("seed = want 201")
	}

	listURL := node.server.URL + "/v1/repositories/repo-1/objects"
	status, payload := clusterCall(t, http.MethodGet, listURL, testClusterSecret, nil)
	if status != http.StatusOK || !strings.Contains(string(payload), objectID) {
		t.Fatalf("cluster list = %d %s, want id list", status, payload)
	}
	if status, _ = call(t, http.MethodGet, listURL, readerToken, nil); status != http.StatusNotFound {
		t.Fatalf("github list = %d, want 404 (cluster-only route)", status)
	}
}

func TestAntiEntropyRepairsMissingObjectsIdempotently(t *testing.T) {
	ctx := context.Background()
	github := newPermissiveGitHub(t)
	nodeB := newClusterNode(t, github, nil, 1)
	firstID, firstContents := testObject("repair target one")
	secondID, secondContents := testObject("repair target two")
	for _, seed := range []struct {
		id       string
		contents []byte
	}{{firstID, firstContents}, {secondID, secondContents}} {
		if status, _ := clusterCall(t, http.MethodPut, objectURL(nodeB.server, "repo-1", seed.id), testClusterSecret, seed.contents); status != http.StatusCreated {
			t.Fatalf("seed node-b = want 201")
		}
	}

	nodeA := newClusterNode(t, github, nil, 1)
	provider := &staticPeerProvider{self: Peer{NodeID: "node-a", BaseURL: "unused"}, peers: []Peer{{NodeID: "node-b", BaseURL: nodeB.server.URL}}}
	repair, err := NewAntiEntropy(nodeA.store, provider, testClusterSecret, []string{"repo-1"})
	if err != nil {
		t.Fatalf("NewAntiEntropy() error = %v", err)
	}

	repaired, err := repair.RunOnce(ctx)
	if err != nil || repaired != 2 {
		t.Fatalf("RunOnce() = %d, %v, want 2 repaired objects", repaired, err)
	}
	for _, want := range []struct {
		id       string
		contents []byte
	}{{firstID, firstContents}, {secondID, secondContents}} {
		got, err := nodeA.store.ReadObject(ctx, "repo-1", want.id)
		if err != nil || string(got) != string(want.contents) {
			t.Fatalf("ReadObject(%s) after repair = %q, %v", want.id, got, err)
		}
	}

	repaired, err = repair.RunOnce(ctx)
	if err != nil || repaired != 0 {
		t.Fatalf("RunOnce(second) = %d, %v, want 0 (idempotent)", repaired, err)
	}
}

func startRuntimeNode(t *testing.T, github *httptest.Server, dbPath, nodeID string) (*httptest.Server, *ClusterRuntime) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	store, err := OpenStorage(t.TempDir(), dbPath)
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime, err := NewClusterRuntime(store, Config{
		GitHubAPIBaseURL: github.URL,
		Repositories: map[string]RepositoryConfig{
			"repo-1": {GitHubOwner: "acme", GitHubRepo: "thread-keep"},
		},
		Cluster: &ClusterConfig{NodeID: nodeID, AdvertiseURL: "http://" + listener.Addr().String()},
	}, testClusterSecret)
	if err != nil {
		t.Fatalf("NewClusterRuntime() error = %v", err)
	}
	server := httptest.NewUnstartedServer(runtime.Handler)
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return server, runtime
}

func TestClusterRuntimeTwoNodesReplicateViaDBLease(t *testing.T) {
	ctx := context.Background()
	github := newPermissiveGitHub(t)
	dbPath := filepath.Join(t.TempDir(), "cluster.db")
	serverA, runtimeA := startRuntimeNode(t, github, dbPath, "node-a")
	serverB, runtimeB := startRuntimeNode(t, github, dbPath, "node-b")

	if runtimeA.AntiEntropyInterval != 300*time.Second {
		t.Fatalf("runtime default anti-entropy interval = %v, want 300s", runtimeA.AntiEntropyInterval)
	}
	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := runtimeA.Membership.Start(startCtx); err != nil {
		t.Fatalf("Membership.Start(a) error = %v", err)
	}
	if err := runtimeB.Membership.Start(startCtx); err != nil {
		t.Fatalf("Membership.Start(b) error = %v", err)
	}

	objectID, contents := testObject("db-lease replicated object")
	if status, _ := call(t, http.MethodPut, objectURL(serverA, "repo-1", objectID), writerToken, contents); status != http.StatusCreated {
		t.Fatalf("external PUT via node-a = want 201")
	}
	status, payload := clusterCall(t, http.MethodGet, objectURL(serverB, "repo-1", objectID), testClusterSecret, nil)
	if status != http.StatusOK || string(payload) != string(contents) {
		t.Fatalf("node-b local copy = %d %q, want write-through replica", status, payload)
	}
}

func TestClusterRuntimeValidatesConfiguration(t *testing.T) {
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base := Config{GitHubAPIBaseURL: "https://api.github.test", Repositories: map[string]RepositoryConfig{"repo-1": {GitHubOwner: "acme", GitHubRepo: "thread-keep"}}}

	noCluster := base
	if _, err := NewClusterRuntime(store, noCluster, testClusterSecret); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("NewClusterRuntime(no cluster section) error = %v, want validation", err)
	}
	badScheme := base
	badScheme.Cluster = &ClusterConfig{NodeID: "node-a", AdvertiseURL: "node-a:8320"}
	if _, err := NewClusterRuntime(store, badScheme, testClusterSecret); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("NewClusterRuntime(bad advertise) error = %v, want validation", err)
	}
	badTTL := base
	badTTL.Cluster = &ClusterConfig{NodeID: "node-a", AdvertiseURL: "http://node-a:8320", HeartbeatSeconds: 30, TTLSeconds: 30}
	if _, err := NewClusterRuntime(store, badTTL, testClusterSecret); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("NewClusterRuntime(ttl<=heartbeat) error = %v, want validation", err)
	}
	if _, err := NewClusterRuntime(store, badTTL, ""); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("NewClusterRuntime(empty secret) error = %v, want validation", err)
	}
}

func TestInternalPutDoesNotReplicate(t *testing.T) {
	github := newPermissiveGitHub(t)
	var peerHits atomic.Int64
	recordingPeer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		peerHits.Add(1)
		writer.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(recordingPeer.Close)
	nodeA := newClusterNode(t, github, []Peer{{NodeID: "recorder", BaseURL: recordingPeer.URL}}, 2)
	objectID, contents := testObject("internal publish")

	if status, _ := clusterCall(t, http.MethodPut, objectURL(nodeA.server, "repo-1", objectID), testClusterSecret, contents); status != http.StatusCreated {
		t.Fatalf("internal PUT = want 201")
	}
	if peerHits.Load() != 0 {
		t.Fatalf("internal PUT fanned out to %d peers, want 0", peerHits.Load())
	}
}
