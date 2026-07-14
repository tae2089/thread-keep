package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/tae2089/thread-keep/internal/domain"
)

type packFingerprintEntry struct {
	Name    string
	Size    int64
	ModTime int64
}

type packCatalog struct {
	directory   string
	fingerprint []packFingerprintEntry
	indexes     []packIndex
}

type packCatalogCache struct {
	mu          sync.Mutex
	entries     map[string]*packCatalog
	generations map[string]uint64
	readFile    func(string) ([]byte, error)
}

func newPackCatalogCache() *packCatalogCache {
	return &packCatalogCache{
		entries:     make(map[string]*packCatalog),
		generations: make(map[string]uint64),
		readFile:    os.ReadFile,
	}
}

func (c *packCatalogCache) load(repositoryID, directory string) (*packCatalog, error) {
	for {
		fingerprint, err := scanPackFingerprint(directory)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		if cached := c.entries[repositoryID]; cached != nil && slices.Equal(cached.fingerprint, fingerprint) {
			c.mu.Unlock()
			return cached, nil
		}
		generation := c.generations[repositoryID]
		c.mu.Unlock()

		indexes, err := loadPackCatalogIndexes(directory, fingerprint, c.readFile)
		loadErr := err
		publishFingerprint, err := scanPackFingerprint(directory)
		if err != nil {
			return nil, err
		}
		if !slices.Equal(fingerprint, publishFingerprint) {
			continue
		}
		if loadErr != nil {
			return nil, loadErr
		}
		catalog := &packCatalog{directory: directory, fingerprint: fingerprint, indexes: indexes}
		c.mu.Lock()
		if generation != c.generations[repositoryID] {
			c.mu.Unlock()
			continue
		}
		if cached := c.entries[repositoryID]; cached != nil && slices.Equal(cached.fingerprint, fingerprint) {
			c.mu.Unlock()
			return cached, nil
		}
		c.entries[repositoryID] = catalog
		c.mu.Unlock()
		return catalog, nil
	}
}

func (c *packCatalogCache) invalidate(repositoryID string) {
	c.mu.Lock()
	delete(c.entries, repositoryID)
	c.generations[repositoryID]++
	c.mu.Unlock()
}

func (c *packCatalog) newReadSession() *packReadSession {
	return newPackReadSession(c.directory, c.indexes)
}

func scanPackFingerprint(directory string) ([]packFingerprintEntry, error) {
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("list packs: %w", err))
	}
	fingerprint := make([]packFingerprintEntry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, ".idx.json") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("stat pack index %s: %w", name, err))
		}
		fingerprint = append(fingerprint, packFingerprintEntry{Name: name, Size: info.Size(), ModTime: info.ModTime().UnixNano()})
	}
	sort.Slice(fingerprint, func(i, j int) bool { return fingerprint[i].Name < fingerprint[j].Name })
	return fingerprint, nil
}

func loadPackCatalogIndexes(directory string, fingerprint []packFingerprintEntry, readFile func(string) ([]byte, error)) ([]packIndex, error) {
	return loadPackIndexesWithPolicy(directory, fingerprint, readFile, true)
}

func loadPackIndexesWithPolicy(directory string, fingerprint []packFingerprintEntry, readFile func(string) ([]byte, error), failOnReadError bool) ([]packIndex, error) {
	indexes := make([]packIndex, 0, len(fingerprint))
	for _, fingerprintEntry := range fingerprint {
		contents, err := readFile(filepath.Join(directory, fingerprintEntry.Name))
		if err != nil {
			if failOnReadError {
				return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("read pack index %s: %w", fingerprintEntry.Name, err))
			}
			continue
		}
		var index packIndex
		if json.Unmarshal(contents, &index) != nil || index.Version != packIndexVersion {
			continue
		}
		index.name = strings.TrimSuffix(fingerprintEntry.Name, ".idx.json")
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i].name > indexes[j].name })
	return indexes, nil
}
