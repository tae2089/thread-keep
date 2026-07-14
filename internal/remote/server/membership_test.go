package server

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestDBMembershipStartRegistersAndLeaveRemoves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "cluster.db")
	providerA, _ := openTestLeaseProvider(t, path, "node-a", "http://127.0.0.1:1111")
	providerB, _ := openTestLeaseProvider(t, path, "node-b", "http://127.0.0.1:2222")
	membershipA := newDBMembership(providerA, time.Minute)
	membershipB := newDBMembership(providerB, time.Minute)

	if err := membershipA.Start(ctx); err != nil {
		t.Fatalf("Start(a) error = %v", err)
	}
	if err := membershipB.Start(ctx); err != nil {
		t.Fatalf("Start(b) error = %v", err)
	}
	peers, err := membershipA.Peers(ctx)
	if err != nil || len(peers) != 1 || peers[0].NodeID != "node-b" {
		t.Fatalf("Peers(a) after Start = %+v, %v, want node-b", peers, err)
	}

	if err := membershipB.Leave(ctx); err != nil {
		t.Fatalf("Leave(b) error = %v", err)
	}
	peers, err = membershipA.Peers(ctx)
	if err != nil || len(peers) != 0 {
		t.Fatalf("Peers(a) after Leave(b) = %+v, %v, want empty (active departure)", peers, err)
	}
}

func startSwimNode(t *testing.T, nodeID, baseURL string, seeds []string) *SwimMembership {
	t.Helper()
	membership, err := NewSwimMembership(SwimOptions{
		Self:     Peer{NodeID: nodeID, BaseURL: baseURL},
		BindAddr: "127.0.0.1:0",
		Seeds:    seeds,
		Secret:   testClusterSecret,
	})
	if err != nil {
		t.Fatalf("NewSwimMembership(%s) error = %v", nodeID, err)
	}
	if err := membership.Start(context.Background()); err != nil {
		t.Fatalf("Start(%s) error = %v", nodeID, err)
	}
	t.Cleanup(func() { _ = membership.Leave(context.Background()) })
	return membership
}

func swimGossipAddress(membership *SwimMembership) string {
	node := membership.list.LocalNode()
	return net.JoinHostPort(node.Addr.String(), strconv.Itoa(int(node.Port)))
}

func TestSwimMembershipDiscoversPeersViaGossip(t *testing.T) {
	ctx := context.Background()
	nodeA := startSwimNode(t, "node-a", "http://127.0.0.1:1111", nil)
	nodeB := startSwimNode(t, "node-b", "http://127.0.0.1:2222", []string{swimGossipAddress(nodeA)})

	peersOfA, err := nodeA.Peers(ctx)
	if err != nil || len(peersOfA) != 1 || peersOfA[0] != (Peer{NodeID: "node-b", BaseURL: "http://127.0.0.1:2222"}) {
		t.Fatalf("Peers(a) = %+v, %v, want node-b with advertised base URL", peersOfA, err)
	}
	peersOfB, err := nodeB.Peers(ctx)
	if err != nil || len(peersOfB) != 1 || peersOfB[0].NodeID != "node-a" {
		t.Fatalf("Peers(b) = %+v, %v, want node-a", peersOfB, err)
	}
}

func TestSwimMembershipLeavePropagates(t *testing.T) {
	ctx := context.Background()
	nodeA := startSwimNode(t, "node-a", "http://127.0.0.1:1111", nil)
	nodeB := startSwimNode(t, "node-b", "http://127.0.0.1:2222", []string{swimGossipAddress(nodeA)})

	if err := nodeB.Leave(ctx); err != nil {
		t.Fatalf("Leave(b) error = %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		peers, err := nodeA.Peers(ctx)
		if err != nil {
			t.Fatalf("Peers(a) error = %v", err)
		}
		if len(peers) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Peers(a) after Leave(b) = %+v, want empty", peers)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestClusterRuntimeSelectsMembershipMode(t *testing.T) {
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base := Config{GitHubAPIBaseURL: "https://api.github.test", Repositories: map[string]RepositoryConfig{"repo-1": {GitHubOwner: "acme", GitHubRepo: "thread-keep"}}}

	swimNoBind := base
	swimNoBind.Cluster = &ClusterConfig{NodeID: "node-a", AdvertiseURL: "http://node-a:8320", Membership: "swim"}
	if _, err := NewClusterRuntime(store, swimNoBind, testClusterSecret); err == nil {
		t.Fatalf("NewClusterRuntime(swim without bind_addr) expected validation error")
	}

	unknown := base
	unknown.Cluster = &ClusterConfig{NodeID: "node-a", AdvertiseURL: "http://node-a:8320", Membership: "raft"}
	if _, err := NewClusterRuntime(store, unknown, testClusterSecret); err == nil {
		t.Fatalf("NewClusterRuntime(unknown membership) expected validation error")
	}

	swim := base
	swim.Cluster = &ClusterConfig{NodeID: "node-a", AdvertiseURL: "http://node-a:8320", Membership: "swim", Swim: &SwimConfig{BindAddr: "127.0.0.1:0"}}
	runtime, err := NewClusterRuntime(store, swim, testClusterSecret)
	if err != nil {
		t.Fatalf("NewClusterRuntime(swim) error = %v", err)
	}
	if _, ok := runtime.Membership.(*SwimMembership); !ok {
		t.Fatalf("Membership = %T, want *SwimMembership", runtime.Membership)
	}

	db := base
	db.Cluster = &ClusterConfig{NodeID: "node-a", AdvertiseURL: "http://node-a:8320"}
	runtime, err = NewClusterRuntime(store, db, testClusterSecret)
	if err != nil {
		t.Fatalf("NewClusterRuntime(db default) error = %v", err)
	}
	if _, ok := runtime.Membership.(*dbMembership); !ok {
		t.Fatalf("Membership = %T, want *dbMembership (default)", runtime.Membership)
	}
}
