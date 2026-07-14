package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

type GCConfig struct {
	Auto            *bool `json:"auto,omitempty"`
	AutoThreshold   int   `json:"auto_threshold,omitempty"`
	IntervalSeconds int   `json:"interval_seconds,omitempty"`
	GraceSeconds    int   `json:"grace_seconds,omitempty"`
}

type MaintenancePolicy struct {
	Auto          bool
	AutoThreshold int
	Interval      time.Duration
	Grace         time.Duration
}

type GCRepositoryResult struct {
	Kept    int  `json:"kept"`
	Deleted int  `json:"deleted"`
	Packed  int  `json:"packed"`
	Aborted bool `json:"aborted"`
}

type GCResult struct {
	Repositories map[string]GCRepositoryResult `json:"repositories"`
	Skipped      bool                          `json:"skipped,omitempty"`
}

type objectParents struct {
	ParentID       string   `json:"parent_id"`
	ParentIDs      []string `json:"parent_ids"`
	LegacyParentID string   `json:"legacy_parent_id"`
}

type Maintainer struct {
	store  *CompositeStorage
	policy MaintenancePolicy
	busy   atomic.Bool
}

type MaintainedStorage struct {
	Storage
	maintainer *Maintainer
}

type repackHooks struct {
	stat      func(string) (os.FileInfo, error)
	packWrite packWriteHooks
}

var errRepackAborted = errors.New("repack aborted because a retained object could not be read")

func ResolveMaintenancePolicy(config *GCConfig) (MaintenancePolicy, error) {
	policy := MaintenancePolicy{Auto: true, AutoThreshold: 512, Grace: 14 * 24 * time.Hour}
	if config == nil {
		return policy, nil
	}
	if config.Auto != nil {
		policy.Auto = *config.Auto
	}
	if config.AutoThreshold != 0 {
		if config.AutoThreshold < 1 {
			return MaintenancePolicy{}, domain.NewError(domain.CodeValidation, errors.New("gc.auto_threshold must be positive"))
		}
		policy.AutoThreshold = config.AutoThreshold
	}
	if config.GraceSeconds != 0 {
		if config.GraceSeconds < 60 {
			return MaintenancePolicy{}, domain.NewError(domain.CodeValidation, errors.New("gc.grace_seconds must be at least 60"))
		}
		policy.Grace = time.Duration(config.GraceSeconds) * time.Second
	}
	if config.IntervalSeconds != 0 {
		if config.IntervalSeconds < 1 {
			return MaintenancePolicy{}, domain.NewError(domain.CodeValidation, errors.New("gc.interval_seconds must be positive"))
		}
		policy.Interval = time.Duration(config.IntervalSeconds) * time.Second
	}
	if !policy.Auto {
		policy.Interval = 0
	}
	return policy, nil
}

func NewMaintainer(store *CompositeStorage, policy MaintenancePolicy) *Maintainer {
	return &Maintainer{store: store, policy: policy}
}

func (m *Maintainer) Wrap(store Storage) *MaintainedStorage {
	return &MaintainedStorage{Storage: store, maintainer: m}
}

func (m *Maintainer) NotifyPublish(repositoryID string) {
	if !m.policy.Auto {
		return
	}
	go m.maybeRun(repositoryID)
}

func (m *Maintainer) maybeRun(repositoryID string) {
	if !m.busy.CompareAndSwap(false, true) {
		return
	}
	defer m.busy.Store(false)
	ctx := context.Background()
	loose, err := m.store.objects.listLooseObjects(ctx, repositoryID)
	if err != nil || len(loose) <= m.policy.AutoThreshold {
		return
	}
	_, _ = RunGC(ctx, m.store, []string{repositoryID}, m.policy.Grace)
}

func (m *MaintainedStorage) PublishObject(ctx context.Context, repositoryID, objectID string, contents []byte) (bool, error) {
	created, err := m.Storage.PublishObject(ctx, repositoryID, objectID, contents)
	if err == nil {
		m.maintainer.NotifyPublish(repositoryID)
	}
	return created, err
}

func RunPeriodicGC(ctx context.Context, store *CompositeStorage, repositories []string, grace, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = RunGC(ctx, store, repositories, grace)
		}
	}
}

func RunGC(ctx context.Context, store *CompositeStorage, repositories []string, grace time.Duration) (GCResult, error) {
	result := GCResult{Repositories: make(map[string]GCRepositoryResult, len(repositories))}
	lock, acquired, err := acquireMaintenanceLock(store.objects.root)
	if err != nil {
		return GCResult{}, err
	}
	if !acquired {
		result.Skipped = true
		return result, nil
	}
	defer func() {
		_ = unlockMaintenanceFile(lock)
		_ = lock.Close()
	}()
	for _, repositoryID := range repositories {
		repositoryResult, err := gcRepository(ctx, store, repositoryID, grace)
		if err != nil {
			return GCResult{}, err
		}
		result.Repositories[repositoryID] = repositoryResult
	}
	return result, nil
}

func acquireMaintenanceLock(root string) (*os.File, bool, error) {
	lock, err := os.OpenFile(filepath.Join(root, ".maintenance.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("open maintenance lock: %w", err))
	}
	acquired, err := tryLockMaintenanceFile(lock)
	if err != nil {
		_ = lock.Close()
		return nil, false, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("acquire maintenance lock: %w", err))
	}
	if !acquired {
		_ = lock.Close()
		return nil, false, nil
	}
	return lock, true, nil
}

func gcRepository(ctx context.Context, store *CompositeStorage, repositoryID string, grace time.Duration) (GCRepositoryResult, error) {
	refs, err := store.refs.ListRefs(ctx, repositoryID)
	if err != nil {
		return GCRepositoryResult{}, err
	}
	ids, err := store.ListObjects(ctx, repositoryID)
	if err != nil {
		return GCRepositoryResult{}, err
	}
	reachable := make(map[string]bool)
	queue := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.CommitID != "" {
			queue = append(queue, ref.CommitID)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if reachable[id] {
			continue
		}
		contents, err := store.ReadObject(ctx, repositoryID, id)
		if isMissingObjectError(err) {
			return GCRepositoryResult{Kept: len(ids), Aborted: true}, nil
		}
		if err != nil {
			return GCRepositoryResult{}, err
		}
		reachable[id] = true
		var parents objectParents
		if json.Unmarshal(contents, &parents) == nil {
			queue = append(queue, parents.ParentIDs...)
			if parents.ParentID != "" {
				queue = append(queue, parents.ParentID)
			}
			if parents.LegacyParentID != "" {
				queue = append(queue, parents.LegacyParentID)
			}
		}
	}
	cutoff := time.Now().Add(-grace)
	repositoryResult := GCRepositoryResult{}
	loose, err := store.objects.listLooseObjects(ctx, repositoryID)
	if err != nil {
		return GCRepositoryResult{}, err
	}
	agedReachableLoose := make([]string, 0, len(loose))
	for _, id := range loose {
		path := store.objects.loosePath(repositoryID, id)
		info, statErr := os.Stat(path)
		aged := statErr == nil && !info.ModTime().After(cutoff)
		if reachable[id] {
			if aged {
				agedReachableLoose = append(agedReachableLoose, id)
			}
			continue
		}
		if !aged {
			continue
		}
		if os.Remove(path) == nil {
			repositoryResult.Deleted++
		}
	}
	packedDropped, packedMoved, err := repackRepository(ctx, store, repositoryID, reachable, agedReachableLoose, cutoff)
	if errors.Is(err, errRepackAborted) {
		repositoryResult.Aborted = true
		repositoryResult.Kept = len(ids) - repositoryResult.Deleted
		return repositoryResult, nil
	}
	if err != nil {
		return GCRepositoryResult{}, err
	}
	repositoryResult.Deleted += packedDropped
	repositoryResult.Packed = packedMoved
	repositoryResult.Kept = len(ids) - repositoryResult.Deleted
	return repositoryResult, nil
}

func repackRepository(ctx context.Context, store *CompositeStorage, repositoryID string, reachable map[string]bool, agedReachableLoose []string, cutoff time.Time) (int, int, error) {
	return repackRepositoryWithHooks(ctx, store, repositoryID, reachable, agedReachableLoose, cutoff, repackHooks{})
}

func repackRepositoryWithHooks(ctx context.Context, store *CompositeStorage, repositoryID string, reachable map[string]bool, agedReachableLoose []string, cutoff time.Time, hooks repackHooks) (int, int, error) {
	if hooks.stat == nil {
		hooks.stat = os.Stat
	}
	packsDirectory := store.objects.packsDirectory(repositoryID)
	catalog, err := store.objects.loadPackCatalog(repositoryID)
	if err != nil {
		return 0, 0, err
	}
	indexes := catalog.indexes
	readSession := catalog.newReadSession()
	defer readSession.Close()
	dropped := 0
	rewriteNeeded := len(agedReachableLoose) > 0 || len(indexes) > 1
	sources := make([]packObjectSource, 0)
	selected := make(map[string]struct{})
	for _, index := range indexes {
		index := index
		info, statErr := hooks.stat(packDataPath(packsDirectory, index.name))
		for id, entry := range index.Objects {
			id := id
			entry := entry
			drop := !reachable[id] && packEntryAged(entry, info, statErr, cutoff)
			if drop {
				rewriteNeeded = true
			}
			if _, ok := selected[id]; ok {
				continue
			}
			if drop {
				dropped++
				continue
			}
			storedAt := entry.StoredAt
			if storedAt == 0 && statErr == nil {
				storedAt = info.ModTime().Unix()
			}
			selected[id] = struct{}{}
			sources = append(sources, packObjectSource{
				ID:       id,
				Size:     entry.RawSize,
				StoredAt: storedAt,
				Read: func() ([]byte, error) {
					contents, found, readErr := readSession.readFromIndex(index, id)
					if readErr != nil || !found {
						return nil, errRepackAborted
					}
					return contents, nil
				},
			})
		}
	}
	moved := 0
	for _, id := range agedReachableLoose {
		id := id
		if _, ok := selected[id]; ok {
			continue
		}
		path := store.objects.loosePath(repositoryID, id)
		info, statErr := hooks.stat(path)
		storedAt := int64(0)
		size := int64(0)
		if statErr == nil {
			storedAt = info.ModTime().Unix()
			size = info.Size()
		}
		selected[id] = struct{}{}
		sources = append(sources, packObjectSource{
			ID:       id,
			Size:     size,
			StoredAt: storedAt,
			Read: func() ([]byte, error) {
				contents, readErr := os.ReadFile(path)
				if readErr != nil {
					return nil, errRepackAborted
				}
				return contents, nil
			},
		})
		moved++
	}
	if !rewriteNeeded {
		return 0, 0, nil
	}
	for i := range sources {
		if sources[i].Size != 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		contents, readErr := sources[i].Read()
		if readErr != nil {
			if errors.Is(readErr, errRepackAborted) {
				return 0, 0, errRepackAborted
			}
			return 0, 0, readErr
		}
		sources[i].Size = int64(len(contents))
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if len(sources) > 0 {
		if _, err := writePackSourcesContextWithHooks(ctx, packsDirectory, sources, hooks.packWrite); err != nil {
			if errors.Is(err, errRepackAborted) {
				return 0, 0, errRepackAborted
			}
			return 0, 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	store.objects.packCatalogCache.invalidate(repositoryID)
	for _, index := range indexes {
		_ = os.Remove(packDataPath(packsDirectory, index.name))
		_ = os.Remove(packIndexPath(packsDirectory, index.name))
	}
	for _, id := range agedReachableLoose {
		_ = os.Remove(store.objects.loosePath(repositoryID, id))
	}
	store.objects.packCatalogCache.invalidate(repositoryID)
	return dropped, moved, nil
}

func packEntryAged(entry packEntry, packInfo os.FileInfo, statErr error, cutoff time.Time) bool {
	if entry.StoredAt != 0 {
		return entry.StoredAt <= cutoff.Unix()
	}
	return statErr == nil && !packInfo.ModTime().After(cutoff)
}
