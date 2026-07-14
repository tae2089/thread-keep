package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/zeebo/blake3"
)

type FileSystem struct {
	root string
}

type Ref struct {
	RefName   string `json:"ref_name"`
	CommitID  string `json:"commit_id,omitempty"`
	SourceSHA string `json:"source_sha,omitempty"`
	Version   int    `json:"version"`
}

func Open(root string) (*FileSystem, error) {
	if !filepath.IsAbs(root) {
		return nil, domain.NewError(domain.CodeValidation, errors.New("remote path must be absolute"))
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, storageError("inspect remote path", err)
	}
	if !info.IsDir() {
		return nil, domain.NewError(domain.CodeValidation, errors.New("remote path must be a directory"))
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, storageError("resolve remote path", err)
	}
	return &FileSystem{root: resolved}, nil
}

func (f *FileSystem) ReadObject(ctx context.Context, id string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := f.objectPath(id)
	if err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, domain.NewError(domain.CodeObjectMissing, fmt.Errorf("remote object %s is missing", id))
	}
	if err != nil {
		return nil, storageError("read remote object", err)
	}
	if err := validateObjectBytes(id, contents); err != nil {
		return nil, err
	}
	return contents, nil
}

func (f *FileSystem) PublishObject(ctx context.Context, id string, contents []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path, err := f.objectPath(id)
	if err != nil {
		return false, err
	}
	if err := validateObjectBytes(id, contents); err != nil {
		return false, err
	}
	if err := f.ensureDirectories(); err != nil {
		return false, err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, contents) {
			now := time.Now()
			_ = os.Chtimes(path, now, now)
			return false, nil
		}
		return false, domain.NewError(domain.CodeValidation, fmt.Errorf("remote object %s already exists with different contents", id))
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, storageError("read existing remote object", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".object-*.tmp")
	if err != nil {
		return false, storageError("create remote object temp file", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return false, storageError("write remote object", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return false, storageError("sync remote object", err)
	}
	if err := temporary.Close(); err != nil {
		return false, storageError("close remote object", err)
	}
	if err := os.Link(temporaryName, path); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, contents) {
			now := time.Now()
			_ = os.Chtimes(path, now, now)
			return false, nil
		}
		if errors.Is(err, os.ErrExist) {
			return false, domain.NewError(domain.CodeValidation, fmt.Errorf("remote object %s already exists with different contents", id))
		}
		return false, storageError("publish remote object", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return false, err
	}
	return true, nil
}

func (f *FileSystem) ReadRef(ctx context.Context, refName string) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	path, err := f.refPath(refName)
	if err != nil {
		return Ref{}, err
	}
	ref, err := readRef(path, refName)
	if errors.Is(err, os.ErrNotExist) {
		return Ref{RefName: refName}, nil
	}
	if err != nil {
		return Ref{}, err
	}
	return ref, nil
}

func (f *FileSystem) CompareAndSwapRef(ctx context.Context, refName string, expected, next Ref) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	path, err := f.refPath(refName)
	if err != nil {
		return Ref{}, err
	}
	if expected.RefName != refName || next.RefName != refName || next.CommitID == "" || next.SourceSHA == "" || next.Version < 1 {
		return Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote ref compare-and-swap input is invalid"))
	}
	if err := f.ensureDirectories(); err != nil {
		return Ref{}, err
	}
	release, err := acquireLock(path + ".lock")
	if err != nil {
		return Ref{}, err
	}
	defer release()
	current, err := readRef(path, refName)
	if errors.Is(err, os.ErrNotExist) {
		current = Ref{RefName: refName}
	} else if err != nil {
		return Ref{}, err
	}
	if current != expected {
		return Ref{}, domain.NewError(domain.CodeRemoteConflict, errors.New("remote ref changed before compare-and-swap"))
	}
	if next.Version != current.Version+1 {
		return Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote ref version must advance by one"))
	}
	contents, err := json.Marshal(next)
	if err != nil {
		return Ref{}, storageError("serialize remote ref", err)
	}
	if err := WriteAtomic(path, contents); err != nil {
		return Ref{}, err
	}
	return next, nil
}

func (f *FileSystem) objectPath(id string) (string, error) {
	id, err := domain.NormalizeContextCommitID(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(f.root, "objects", id+".json"), nil
}

func (f *FileSystem) refPath(refName string) (string, error) {
	if refName == "" {
		return "", domain.NewError(domain.CodeValidation, errors.New("remote ref name must not be empty"))
	}
	digest := sha256.Sum256([]byte(refName))
	return filepath.Join(f.root, "refs", fmt.Sprintf("%x.json", digest[:])), nil
}

func (f *FileSystem) ensureDirectories() error {
	for _, directory := range []string{filepath.Join(f.root, "objects"), filepath.Join(f.root, "refs")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return storageError("create remote storage", err)
		}
	}
	return nil
}

func validateObjectBytes(id string, contents []byte) error {
	id, err := domain.NormalizeContextCommitID(id)
	if err != nil {
		return err
	}
	digest := blake3.Sum256(contents)
	if fmt.Sprintf("%x", digest[:]) != id {
		return domain.NewError(domain.CodeValidation, fmt.Errorf("remote object %s does not match its content ID", id))
	}
	return nil
}

func readRef(path, refName string) (Ref, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Ref{}, err
		}
		return Ref{}, storageError("read remote ref", err)
	}
	var ref Ref
	if err := json.Unmarshal(contents, &ref); err != nil {
		return Ref{}, domain.NewError(domain.CodeValidation, fmt.Errorf("decode remote ref: %w", err))
	}
	if ref.RefName != refName || ref.CommitID == "" || ref.SourceSHA == "" || ref.Version < 1 {
		return Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote ref is invalid"))
	}
	if _, err := domain.NormalizeContextCommitID(ref.CommitID); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

func acquireLock(path string) (func(), error) {
	lock, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil, domain.NewError(domain.CodeRemoteConflict, errors.New("remote ref is being updated"))
	}
	if err != nil {
		return nil, storageError("lock remote ref", err)
	}
	return func() {
		_ = lock.Close()
		_ = os.Remove(path)
	}, nil
}

// WriteAtomic replaces path with contents and durably publishes the rename.
func WriteAtomic(path string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".ref-*.tmp")
	if err != nil {
		return storageError("create remote ref temp file", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return storageError("write remote ref", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return storageError("sync remote ref", err)
	}
	if err := temporary.Close(); err != nil {
		return storageError("close remote ref", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return storageError("publish remote ref", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return storageError("open remote storage directory", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return storageError("sync remote storage directory", err)
	}
	return nil
}

func storageError(action string, err error) error {
	if err == nil {
		return nil
	}
	if domain.CodeOf(err) != "" {
		return err
	}
	return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("%s: %w", action, err))
}
