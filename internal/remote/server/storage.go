package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
)

type Storage interface {
	ReadObject(ctx context.Context, repositoryID, objectID string) ([]byte, error)
	PublishObject(ctx context.Context, repositoryID, objectID string, contents []byte) (bool, error)
	ListObjects(ctx context.Context, repositoryID string) ([]string, error)
	ReadRef(ctx context.Context, repositoryID, refName string) (remote.Ref, error)
	CompareAndSwapRef(ctx context.Context, repositoryID, refName string, expected, next remote.Ref) (remote.Ref, error)
}

type FileStorage struct {
	root             string
	packCatalogCache *packCatalogCache
}

var _ Storage = (*FileStorage)(nil)

func NewFileStorage(root string) (*FileStorage, error) {
	if !filepath.IsAbs(root) {
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("storage root must be an absolute path"))
	}
	return &FileStorage{root: root, packCatalogCache: newPackCatalogCache()}, nil
}

func (f *FileStorage) ReadObject(ctx context.Context, repositoryID, objectID string) ([]byte, error) {
	repository, err := f.open(repositoryID)
	if err != nil {
		return nil, err
	}
	contents, err := repository.ReadObject(ctx, objectID)
	if err == nil || !isMissingObjectError(err) {
		return contents, err
	}
	catalog, indexErr := f.loadPackCatalog(repositoryID)
	if indexErr != nil {
		return nil, indexErr
	}
	session := catalog.newReadSession()
	defer session.Close()
	packed, found, packErr := session.readObject(objectID)
	if packErr != nil {
		return nil, packErr
	}
	if found {
		return packed, nil
	}
	return nil, err
}

func (f *FileStorage) PublishObject(ctx context.Context, repositoryID, objectID string, contents []byte) (bool, error) {
	repository, err := f.open(repositoryID)
	if err != nil {
		return false, err
	}
	return repository.PublishObject(ctx, objectID, contents)
}

func (f *FileStorage) ListObjects(ctx context.Context, repositoryID string) ([]string, error) {
	loose, err := f.listLooseObjects(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(loose))
	ids := make([]string, 0, len(loose))
	for _, id := range loose {
		seen[id] = true
		ids = append(ids, id)
	}
	catalog, err := f.loadPackCatalog(repositoryID)
	if err != nil {
		return nil, err
	}
	for _, index := range catalog.indexes {
		for id := range index.Objects {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func (f *FileStorage) listLooseObjects(ctx context.Context, repositoryID string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(f.root, repositoryID, "objects"))
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("list repository objects: %w", err))
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && strings.HasSuffix(name, ".json") {
			ids = append(ids, strings.TrimSuffix(name, ".json"))
		}
	}
	return ids, nil
}

func (f *FileStorage) packsDirectory(repositoryID string) string {
	return filepath.Join(f.root, repositoryID, "packs")
}

func (f *FileStorage) loadPackCatalog(repositoryID string) (*packCatalog, error) {
	return f.packCatalogCache.load(repositoryID, f.packsDirectory(repositoryID))
}

func (f *FileStorage) loosePath(repositoryID, objectID string) string {
	return filepath.Join(f.root, repositoryID, "objects", objectID+".json")
}

func (f *FileStorage) ReadRef(ctx context.Context, repositoryID, refName string) (remote.Ref, error) {
	repository, err := f.open(repositoryID)
	if err != nil {
		return remote.Ref{}, err
	}
	return repository.ReadRef(ctx, refName)
}

func (f *FileStorage) CompareAndSwapRef(ctx context.Context, repositoryID, refName string, expected, next remote.Ref) (remote.Ref, error) {
	repository, err := f.open(repositoryID)
	if err != nil {
		return remote.Ref{}, err
	}
	return repository.CompareAndSwapRef(ctx, refName, expected, next)
}

func (f *FileStorage) open(repositoryID string) (*remote.FileSystem, error) {
	directory := filepath.Join(f.root, repositoryID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create repository storage: %w", err))
	}
	return remote.Open(directory)
}
