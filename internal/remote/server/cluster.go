package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
)

const (
	peerControlTimeout           = 15 * time.Second
	peerTransferBaseTimeout      = 60 * time.Second
	peerTransferTimeoutPerMiB    = 2 * time.Second
	peerTransferTimeoutCap       = 15 * time.Minute
	peerDownloadTimeout          = peerTransferTimeoutCap
	peerTransferTimeoutScaleSize = 1 << 20
)

type peerClient struct {
	secret string
	client *http.Client
}

type ReplicatedStorage struct {
	inner             Storage
	provider          PeerProvider
	client            *peerClient
	replicationFactor int
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

type AntiEntropy struct {
	local        Storage
	provider     PeerProvider
	client       *peerClient
	repositories []string
}

type ClusterRuntime struct {
	Handler             http.Handler
	Storage             Storage
	Membership          Membership
	AntiEntropy         *AntiEntropy
	AntiEntropyInterval time.Duration
}

var _ Storage = (*ReplicatedStorage)(nil)

func newPeerClient(secret string) *peerClient {
	return &peerClient{secret: secret, client: &http.Client{}}
}

func (c *peerClient) do(ctx context.Context, method, target string, body []byte, timeout time.Duration) (*http.Response, error) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(requestCtx, method, target, reader)
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set(clusterSecretHeader, c.secret)
	response, err := c.client.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = &cancelReadCloser{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func peerUploadTimeout(size int) time.Duration {
	if size <= 0 {
		return peerTransferBaseTimeout
	}
	maximumScaledBytes := int64((peerTransferTimeoutCap-peerTransferBaseTimeout)/peerTransferTimeoutPerMiB) * peerTransferTimeoutScaleSize
	if int64(size) >= maximumScaledBytes {
		return peerTransferTimeoutCap
	}
	wholeMiB := int64(size) / peerTransferTimeoutScaleSize
	remainingBytes := int64(size) % peerTransferTimeoutScaleSize
	additional := time.Duration(wholeMiB)*peerTransferTimeoutPerMiB +
		time.Duration(remainingBytes)*peerTransferTimeoutPerMiB/peerTransferTimeoutScaleSize
	return peerTransferBaseTimeout + additional
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (c *peerClient) objectURL(baseURL, repositoryID, objectID string) string {
	return baseURL + routePrefix + url.PathEscape(repositoryID) + "/objects/" + url.PathEscape(objectID)
}

func (c *peerClient) readObject(ctx context.Context, baseURL, repositoryID, objectID string) ([]byte, error) {
	response, err := c.do(ctx, http.MethodGet, c.objectURL(baseURL, repositoryID, objectID), nil, peerDownloadTimeout)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer object read returned status %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, maxObjectRequestBytes))
}

func (c *peerClient) publishObject(ctx context.Context, baseURL, repositoryID, objectID string, contents []byte) error {
	response, err := c.do(ctx, http.MethodPut, c.objectURL(baseURL, repositoryID, objectID), contents, peerUploadTimeout(len(contents)))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return fmt.Errorf("peer object publish returned status %d", response.StatusCode)
	}
	return nil
}

func (c *peerClient) listObjects(ctx context.Context, baseURL, repositoryID string) ([]string, error) {
	response, err := c.do(ctx, http.MethodGet, baseURL+routePrefix+url.PathEscape(repositoryID)+"/objects", nil, peerControlTimeout)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer object list returned status %d", response.StatusCode)
	}
	var ids []string
	if err := decodeJSONLimitedTo(response.Body, &ids, limitedJSONSingleValue, maxObjectListResponseBytes); err != nil {
		return nil, err
	}
	return ids, nil
}

func NewReplicatedStorage(inner Storage, provider PeerProvider, clusterSecret string, replicationFactor int) (*ReplicatedStorage, error) {
	if inner == nil || provider == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("replicated storage requires inner storage and a peer provider"))
	}
	if strings.TrimSpace(clusterSecret) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("replicated storage requires a cluster secret"))
	}
	if replicationFactor < 1 {
		return nil, domain.NewError(domain.CodeValidation, errors.New("replication factor must be at least one"))
	}
	return &ReplicatedStorage{inner: inner, provider: provider, client: newPeerClient(clusterSecret), replicationFactor: replicationFactor}, nil
}

func (r *ReplicatedStorage) ListObjects(ctx context.Context, repositoryID string) ([]string, error) {
	return r.inner.ListObjects(ctx, repositoryID)
}

func (r *ReplicatedStorage) ReadRef(ctx context.Context, repositoryID, refName string) (remote.Ref, error) {
	return r.inner.ReadRef(ctx, repositoryID, refName)
}

func (r *ReplicatedStorage) CompareAndSwapRef(ctx context.Context, repositoryID, refName string, expected, next remote.Ref) (remote.Ref, error) {
	return r.inner.CompareAndSwapRef(ctx, repositoryID, refName, expected, next)
}

func (r *ReplicatedStorage) ReadObject(ctx context.Context, repositoryID, objectID string) ([]byte, error) {
	contents, err := r.inner.ReadObject(ctx, repositoryID, objectID)
	if err == nil {
		return contents, nil
	}
	if !isMissingObjectError(err) {
		return nil, err
	}
	peers, peersErr := r.provider.Peers(ctx)
	if peersErr != nil {
		return nil, err
	}
	for _, peer := range peers {
		fetched, fetchErr := r.client.readObject(ctx, peer.BaseURL, repositoryID, objectID)
		if fetchErr != nil {
			continue
		}
		if remote.ValidateObjectBytes(objectID, fetched) != nil {
			continue
		}
		_, _ = r.inner.PublishObject(ctx, repositoryID, objectID, fetched)
		return fetched, nil
	}
	return nil, err
}

func (r *ReplicatedStorage) PublishObject(ctx context.Context, repositoryID, objectID string, contents []byte) (bool, error) {
	created, err := r.inner.PublishObject(ctx, repositoryID, objectID, contents)
	if err != nil {
		return false, err
	}
	peers, peersErr := r.provider.Peers(ctx)
	if peersErr != nil {
		return false, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("cluster membership is unavailable; local copy retained, retry is idempotent: %w", peersErr))
	}
	required := min(r.replicationFactor, 1+len(peers))
	copies := 1
	if len(peers) > 0 && required > 1 {
		var acknowledged atomic.Int64
		var group sync.WaitGroup
		for _, peer := range peers {
			group.Add(1)
			go func(peer Peer) {
				defer group.Done()
				if r.client.publishObject(ctx, peer.BaseURL, repositoryID, objectID, contents) == nil {
					acknowledged.Add(1)
				}
			}(peer)
		}
		group.Wait()
		copies += int(acknowledged.Load())
	}
	if copies < required {
		return false, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("object %s reached %d of %d required copies", objectID, copies, required))
	}
	return created, nil
}

func NewAntiEntropy(local Storage, provider PeerProvider, clusterSecret string, repositories []string) (*AntiEntropy, error) {
	if local == nil || provider == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("anti-entropy requires local storage and a peer provider"))
	}
	if strings.TrimSpace(clusterSecret) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("anti-entropy requires a cluster secret"))
	}
	return &AntiEntropy{local: local, provider: provider, client: newPeerClient(clusterSecret), repositories: append([]string(nil), repositories...)}, nil
}

func (a *AntiEntropy) RunOnce(ctx context.Context) (int, error) {
	peers, err := a.provider.Peers(ctx)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, repositoryID := range a.repositories {
		localIDs, err := a.local.ListObjects(ctx, repositoryID)
		if err != nil {
			continue
		}
		present := make(map[string]bool, len(localIDs))
		for _, id := range localIDs {
			present[id] = true
		}
		for _, peer := range peers {
			remoteIDs, err := a.client.listObjects(ctx, peer.BaseURL, repositoryID)
			if err != nil {
				continue
			}
			for _, id := range remoteIDs {
				if present[id] {
					continue
				}
				contents, err := a.client.readObject(ctx, peer.BaseURL, repositoryID, id)
				if err != nil || remote.ValidateObjectBytes(id, contents) != nil {
					continue
				}
				if _, err := a.local.PublishObject(ctx, repositoryID, id, contents); err != nil {
					continue
				}
				present[id] = true
				repaired++
			}
		}
	}
	return repaired, nil
}

func (a *AntiEntropy) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = a.RunOnce(ctx)
		}
	}
}

func NewClusterRuntime(store *CompositeStorage, config Config, clusterSecret string) (*ClusterRuntime, error) {
	if config.Cluster == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("cluster runtime requires a cluster configuration section"))
	}
	cluster := *config.Cluster
	if cluster.ReplicationFactor == 0 {
		cluster.ReplicationFactor = 2
	}
	if cluster.HeartbeatSeconds == 0 {
		cluster.HeartbeatSeconds = 10
	}
	if cluster.TTLSeconds == 0 {
		cluster.TTLSeconds = 30
	}
	if cluster.AntiEntropySeconds == 0 {
		cluster.AntiEntropySeconds = 300
	}
	if cluster.ReplicationFactor < 1 || cluster.HeartbeatSeconds < 1 || cluster.TTLSeconds <= cluster.HeartbeatSeconds || cluster.AntiEntropySeconds < 1 {
		return nil, domain.NewError(domain.CodeValidation, errors.New("cluster intervals are invalid: TTL must exceed the heartbeat and all values must be positive"))
	}
	if !strings.HasPrefix(cluster.AdvertiseURL, "http://") && !strings.HasPrefix(cluster.AdvertiseURL, "https://") {
		return nil, domain.NewError(domain.CodeValidation, errors.New("cluster advertise URL must be an http(s) URL reachable by peers"))
	}
	self := Peer{NodeID: cluster.NodeID, BaseURL: strings.TrimRight(cluster.AdvertiseURL, "/")}
	var membership Membership
	switch cluster.Membership {
	case "", "db":
		provider, err := NewDBLeaseProvider(store.refs, self, time.Duration(cluster.TTLSeconds)*time.Second)
		if err != nil {
			return nil, err
		}
		membership = newDBMembership(provider, time.Duration(cluster.HeartbeatSeconds)*time.Second)
	case "swim":
		if cluster.Swim == nil || strings.TrimSpace(cluster.Swim.BindAddr) == "" {
			return nil, domain.NewError(domain.CodeValidation, errors.New("swim membership requires cluster.swim.bind_addr"))
		}
		swim, err := NewSwimMembership(SwimOptions{Self: self, BindAddr: cluster.Swim.BindAddr, Seeds: append([]string(nil), cluster.Swim.Seeds...), Secret: clusterSecret})
		if err != nil {
			return nil, err
		}
		membership = swim
	default:
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("cluster membership mode %q is not supported (use \"db\" or \"swim\")", cluster.Membership))
	}
	replicated, err := NewReplicatedStorage(store, membership, clusterSecret, cluster.ReplicationFactor)
	if err != nil {
		return nil, err
	}
	policy, err := ResolveMaintenancePolicy(config.GC)
	if err != nil {
		return nil, err
	}
	serving := Storage(replicated)
	if policy.Auto {
		serving = NewMaintainer(store, policy).Wrap(replicated)
	}
	handler, err := NewClusterHandler(store, serving, clusterSecret, config)
	if err != nil {
		return nil, err
	}
	repositories := make([]string, 0, len(config.Repositories))
	for repositoryID := range config.Repositories {
		repositories = append(repositories, repositoryID)
	}
	repair, err := NewAntiEntropy(store, membership, clusterSecret, repositories)
	if err != nil {
		return nil, err
	}
	return &ClusterRuntime{
		Handler:             handler,
		Storage:             serving,
		Membership:          membership,
		AntiEntropy:         repair,
		AntiEntropyInterval: time.Duration(cluster.AntiEntropySeconds) * time.Second,
	}, nil
}

func isMissingObjectError(err error) bool {
	return domain.CodeOf(err) == domain.CodeObjectMissing
}
