package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
)

const packIndexVersion = 2

type packEntry struct {
	Offset   int64 `json:"offset"`
	Length   int64 `json:"length"`
	StoredAt int64 `json:"stored_at,omitempty"`
	RawSize  int64 `json:"raw_size,omitempty"`
}

type packIndex struct {
	Version    int                  `json:"version"`
	Dictionary string               `json:"dictionary,omitempty"`
	Objects    map[string]packEntry `json:"objects"`

	name string
}

type packObjectSource struct {
	ID       string
	Size     int64
	StoredAt int64
	Read     func() ([]byte, error)
}

type packWriteHooks struct {
	afterSourceRead func()
	writeIndex      func(string, []byte) error
	writeChunk      func(*os.File, []byte) (int, error)
	rename          func(string, string) error
	remove          func(string) error
}

func packDataPath(directory, name string) string {
	return filepath.Join(directory, name+".pack")
}

func packIndexPath(directory, name string) string {
	return filepath.Join(directory, name+".idx.json")
}

func writePackSourcesContextWithHooks(ctx context.Context, directory string, sources []packObjectSource, hooks packWriteHooks) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack directory: %w", err))
	}
	name := fmt.Sprintf("pack-%d", time.Now().UnixNano())
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].ID < sources[j].ID
	})
	dictionaryIndex := -1
	for i := range sources {
		if dictionaryIndex == -1 || sources[i].Size > sources[dictionaryIndex].Size {
			dictionaryIndex = i
		}
	}
	dictionaryID := ""
	var dictionaryContents []byte
	var err error
	if dictionaryIndex >= 0 {
		dictionaryID = sources[dictionaryIndex].ID
		dictionaryContents, err = readPackSource(ctx, sources[dictionaryIndex], hooks.afterSourceRead)
		if err != nil {
			return "", err
		}
	}
	plainEncoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack encoder: %w", err))
	}
	defer plainEncoder.Close()
	var dictionaryEncoder *zstd.Encoder
	if dictionaryID != "" {
		dictionaryEncoder, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression), zstd.WithEncoderDictRaw(0, dictionaryContents))
		if err != nil {
			return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack dictionary encoder: %w", err))
		}
		defer dictionaryEncoder.Close()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(directory, ".pack-*.tmp")
	if err != nil {
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create pack temp file: %w", err))
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	index := packIndex{Version: packIndexVersion, Dictionary: dictionaryID, Objects: make(map[string]packEntry, len(sources))}
	defaultStoredAt := time.Now().Unix()
	offset := int64(0)
	var compressedBuffer []byte
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		contents := dictionaryContents
		if source.ID != dictionaryID {
			contents, err = readPackSource(ctx, source, hooks.afterSourceRead)
			if err != nil {
				return "", err
			}
		}
		var compressed []byte
		if source.ID == dictionaryID {
			compressed = plainEncoder.EncodeAll(contents, compressedBuffer[:0])
		} else {
			compressed = dictionaryEncoder.EncodeAll(contents, compressedBuffer[:0])
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var written int
		var writeErr error
		if hooks.writeChunk != nil {
			written, writeErr = hooks.writeChunk(temporary, compressed)
		} else {
			written, writeErr = temporary.Write(compressed)
		}
		if writeErr != nil {
			return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("write pack entry: %w", writeErr))
		}
		if written != len(compressed) {
			return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("write pack entry: %w", io.ErrShortWrite))
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		storedAt := source.StoredAt
		if storedAt == 0 {
			storedAt = defaultStoredAt
		}
		index.Objects[source.ID] = packEntry{Offset: offset, Length: int64(written), StoredAt: storedAt, RawSize: int64(len(contents))}
		offset += int64(written)
		compressedBuffer = compressed
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("sync pack temp file: %w", err))
	}
	if err := temporary.Close(); err != nil {
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("close pack temp file: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	packPath := packDataPath(directory, name)
	rename := os.Rename
	if hooks.rename != nil {
		rename = hooks.rename
	}
	if err := rename(temporaryName, packPath); err != nil {
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("publish pack: %w", err))
	}
	if err := ctx.Err(); err != nil {
		_ = removePackArtifact(hooks, packPath)
		return "", err
	}
	if err := syncPackDirectory(directory); err != nil {
		_ = removePackArtifact(hooks, packPath)
		return "", err
	}
	if err := ctx.Err(); err != nil {
		_ = removePackArtifact(hooks, packPath)
		return "", err
	}
	indexContents, err := json.Marshal(index)
	if err != nil {
		_ = removePackArtifact(hooks, packPath)
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("serialize pack index: %w", err))
	}
	indexPath := packIndexPath(directory, name)
	writeIndex := remote.WriteAtomic
	if hooks.writeIndex != nil {
		writeIndex = hooks.writeIndex
	}
	if err := ctx.Err(); err != nil {
		_ = removePackArtifact(hooks, packPath)
		return "", err
	}
	if err := writeIndex(indexPath, indexContents); err != nil {
		_ = removePackArtifact(hooks, indexPath)
		_ = removePackArtifact(hooks, packPath)
		return "", domain.NewError(domain.CodeLocalStorage, fmt.Errorf("publish pack index: %w", err))
	}
	return name, nil
}

func removePackArtifact(hooks packWriteHooks, path string) error {
	if hooks.remove != nil {
		return hooks.remove(path)
	}
	return os.Remove(path)
}

func readPackSource(ctx context.Context, source packObjectSource, afterRead func()) ([]byte, error) {
	if source.Read == nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("read pack source %s: missing reader", source.ID))
	}
	contents, err := source.Read()
	if afterRead != nil {
		afterRead()
	}
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("read pack source %s: %w", source.ID, err))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := remote.ValidateObjectBytes(source.ID, contents); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return contents, nil
}

func syncPackDirectory(directory string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, err := os.Open(directory)
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("open pack directory: %w", err))
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("sync pack directory: %w", err))
	}
	return nil
}

func readPackRange(directory, packName string, entry packEntry) ([]byte, error) {
	file, err := os.Open(packDataPath(directory, packName))
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("open pack: %w", err))
	}
	defer file.Close()
	compressed := make([]byte, entry.Length)
	if _, err := file.ReadAt(compressed, entry.Offset); err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("read pack entry: %w", err))
	}
	return compressed, nil
}
