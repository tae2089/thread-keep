package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTestLeaseProvider(t *testing.T, path, nodeID, baseURL string) (*DBLeaseProvider, *GormRefStore) {
	t.Helper()
	store := openTestRefStore(t, path)
	provider, err := NewDBLeaseProvider(store, Peer{NodeID: nodeID, BaseURL: baseURL}, 30*time.Second)
	if err != nil {
		t.Fatalf("NewDBLeaseProvider() error = %v", err)
	}
	return provider, store
}

func TestDBLeaseProviderDiscoversPeersAndExcludesSelf(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cluster.db")
	providerA, _ := openTestLeaseProvider(t, path, "node-a", "http://127.0.0.1:1111")
	providerB, _ := openTestLeaseProvider(t, path, "node-b", "http://127.0.0.1:2222")

	if err := providerA.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat(a) error = %v", err)
	}
	if err := providerB.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat(b) error = %v", err)
	}

	peersOfA, err := providerA.Peers(ctx)
	if err != nil || len(peersOfA) != 1 || peersOfA[0] != (Peer{NodeID: "node-b", BaseURL: "http://127.0.0.1:2222"}) {
		t.Fatalf("Peers(a) = %+v, %v, want only node-b", peersOfA, err)
	}
	peersOfB, err := providerB.Peers(ctx)
	if err != nil || len(peersOfB) != 1 || peersOfB[0].NodeID != "node-a" {
		t.Fatalf("Peers(b) = %+v, %v, want only node-a", peersOfB, err)
	}

	if err := providerA.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat(a, repeat) error = %v", err)
	}
	if peersOfB, err = providerB.Peers(ctx); err != nil || len(peersOfB) != 1 {
		t.Fatalf("Peers(b) after repeated heartbeat = %+v, %v, want still one row", peersOfB, err)
	}
	if providerA.Self() != (Peer{NodeID: "node-a", BaseURL: "http://127.0.0.1:1111"}) {
		t.Fatalf("Self() = %+v", providerA.Self())
	}
}

func TestDBLeaseProviderExpiresStaleNodesByDBClock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cluster.db")
	providerA, store := openTestLeaseProvider(t, path, "node-a", "http://127.0.0.1:1111")
	providerB, _ := openTestLeaseProvider(t, path, "node-b", "http://127.0.0.1:2222")

	if err := providerA.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat(a) error = %v", err)
	}
	if err := providerB.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat(b) error = %v", err)
	}

	if err := store.db.Exec("UPDATE cluster_nodes SET last_seen = datetime('now', '-60 seconds') WHERE node_id = ?", "node-b").Error; err != nil {
		t.Fatalf("age node-b: %v", err)
	}

	peers, err := providerA.Peers(ctx)
	if err != nil || len(peers) != 0 {
		t.Fatalf("Peers(a) with stale node-b = %+v, %v, want empty view", peers, err)
	}

	if err := providerB.Heartbeat(ctx); err != nil {
		t.Fatalf("Heartbeat(b, revive) error = %v", err)
	}
	peers, err = providerA.Peers(ctx)
	if err != nil || len(peers) != 1 || peers[0].NodeID != "node-b" {
		t.Fatalf("Peers(a) after revive = %+v, %v, want node-b back", peers, err)
	}
}
