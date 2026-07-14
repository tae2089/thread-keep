package server

import (
	"context"
	"path/filepath"

	"github.com/tae2089/thread-keep/internal/remote"
)

type CompositeStorage struct {
	objects *FileStorage
	refs    *GormRefStore
}

var _ Storage = (*CompositeStorage)(nil)

func OpenStorage(storageRoot, refDatabaseDSN string) (*CompositeStorage, error) {
	objects, err := NewFileStorage(storageRoot)
	if err != nil {
		return nil, err
	}
	if refDatabaseDSN == "" {
		refDatabaseDSN = filepath.Join(storageRoot, "refs.db")
	}
	refs, err := OpenGormRefStore(refDatabaseDSN)
	if err != nil {
		return nil, err
	}
	return &CompositeStorage{objects: objects, refs: refs}, nil
}

func (c *CompositeStorage) Close() error {
	return c.refs.Close()
}

func (c *CompositeStorage) RefStore() *GormRefStore {
	return c.refs
}

func (c *CompositeStorage) ReadObject(ctx context.Context, repositoryID, objectID string) ([]byte, error) {
	return c.objects.ReadObject(ctx, repositoryID, objectID)
}

func (c *CompositeStorage) PublishObject(ctx context.Context, repositoryID, objectID string, contents []byte) (bool, error) {
	return c.objects.PublishObject(ctx, repositoryID, objectID, contents)
}

func (c *CompositeStorage) ListObjects(ctx context.Context, repositoryID string) ([]string, error) {
	return c.objects.ListObjects(ctx, repositoryID)
}

func (c *CompositeStorage) ReadRef(ctx context.Context, repositoryID, refName string) (remote.Ref, error) {
	return c.refs.ReadRef(ctx, repositoryID, refName)
}

func (c *CompositeStorage) CompareAndSwapRef(ctx context.Context, repositoryID, refName string, expected, next remote.Ref) (remote.Ref, error) {
	return c.refs.CompareAndSwapRef(ctx, repositoryID, refName, expected, next)
}
