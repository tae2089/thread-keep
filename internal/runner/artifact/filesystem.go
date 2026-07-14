package artifact

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
)

type FileStoreConfig struct {
	Root            string
	MaxRequestBytes int
	MaxResultBytes  int
}

type FileStore struct {
	root            string
	maxRequestBytes int
	maxResultBytes  int
}

func NewFileStore(config FileStoreConfig) (*FileStore, error) {
	if !filepath.IsAbs(config.Root) || config.MaxRequestBytes <= 0 || config.MaxResultBytes <= 0 {
		return nil, domain.NewError(domain.CodeValidation, errors.New("runner artifact store configuration is invalid"))
	}
	root := filepath.Clean(config.Root)
	if err := os.MkdirAll(root, 0o2770); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o2770); err != nil {
		return nil, err
	}
	return &FileStore{root: root, maxRequestBytes: config.MaxRequestBytes, maxResultBytes: config.MaxResultBytes}, nil
}

func (s *FileStore) WriteRequest(ctx context.Context, attemptID string, contents []byte) error {
	return s.write(ctx, attemptID, "request.json", contents, s.maxRequestBytes)
}

func (s *FileStore) ReadRequest(ctx context.Context, attemptID string) ([]byte, error) {
	return s.read(ctx, attemptID, "request.json", s.maxRequestBytes)
}

func (s *FileStore) WriteResult(ctx context.Context, attemptID string, contents []byte) error {
	return s.write(ctx, attemptID, "result.json", contents, s.maxResultBytes)
}

func (s *FileStore) ReadResult(ctx context.Context, attemptID string) ([]byte, error) {
	return s.read(ctx, attemptID, "result.json", s.maxResultBytes)
}

func (s *FileStore) Cleanup(ctx context.Context, attemptID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := s.attemptDirectory(attemptID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	return nil
}

func (s *FileStore) write(ctx context.Context, attemptID, name string, contents []byte, limit int) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(contents) == 0 || len(contents) > limit {
		return domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner artifact is empty or exceeds the limit"))
	}
	directory, err := s.attemptDirectory(attemptID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o2770); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o2770); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(0o660); err != nil {
		return err
	}
	if _, err = temporary.Write(contents); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(directory, name))
}

func (s *FileStore) read(ctx context.Context, attemptID, name string, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := s.attemptDirectory(attemptID)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(directory, name))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 || len(contents) > limit {
		return nil, domain.NewError(domain.CodeCoverageIncomplete, errors.New("runner artifact is empty or exceeds the limit"))
	}
	return contents, nil
}

func (s *FileStore) attemptDirectory(attemptID string) (string, error) {
	if len(attemptID) != 64 || attemptID != strings.ToLower(attemptID) {
		return "", domain.NewError(domain.CodeValidation, errors.New("runner attempt ID is invalid"))
	}
	if _, err := hex.DecodeString(attemptID); err != nil {
		return "", domain.NewError(domain.CodeValidation, errors.New("runner attempt ID is invalid"))
	}
	directory := filepath.Join(s.root, attemptID)
	if filepath.Dir(directory) != s.root {
		return "", domain.NewError(domain.CodeValidation, errors.New("runner artifact path escapes the root"))
	}
	return directory, nil
}
