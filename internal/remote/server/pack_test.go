package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
	"github.com/zeebo/blake3"
)

type countingPackDecoder struct {
	packDecoder
	closes *int
}

func (d countingPackDecoder) Close() {
	*d.closes++
	d.packDecoder.Close()
}

func writePack(directory string, objects map[string][]byte, storedAtByObject ...map[string]int64) (string, error) {
	sources := make([]packObjectSource, 0, len(objects))
	for id, contents := range objects {
		id := id
		contents := contents
		storedAt := int64(0)
		if len(storedAtByObject) > 0 {
			storedAt = storedAtByObject[0][id]
		}
		sources = append(sources, packObjectSource{
			ID:       id,
			Size:     int64(len(contents)),
			StoredAt: storedAt,
			Read:     func() ([]byte, error) { return contents, nil },
		})
	}
	return writePackSources(directory, sources)
}

func writePackSources(directory string, sources []packObjectSource) (string, error) {
	return writePackSourcesContextWithHooks(context.Background(), directory, sources, packWriteHooks{})
}

func writePackSourcesWithHooks(directory string, sources []packObjectSource, hooks packWriteHooks) (string, error) {
	return writePackSourcesContextWithHooks(context.Background(), directory, sources, hooks)
}

func loadPackIndexes(directory string) ([]packIndex, error) {
	fingerprint, err := scanPackFingerprint(directory)
	if err != nil {
		return nil, err
	}
	indexes, _ := loadPackIndexesWithPolicy(directory, fingerprint, os.ReadFile, false)
	return indexes, nil
}

func readFromPacks(directory string, indexes []packIndex, objectID string) ([]byte, bool, error) {
	session := newPackReadSession(directory, indexes)
	defer session.Close()
	return session.readObject(objectID)
}

func TestPackSourceWriterRoundTripsObjectsAndPreservesStoredAt(t *testing.T) {
	directory := t.TempDir()
	firstID, firstContents := testObject("first source object")
	secondID, secondContents := testObject("second source object")
	storedAt := map[string]int64{firstID: 111, secondID: 222}
	reads := map[string]int{firstID: 0, secondID: 0}
	sources := []packObjectSource{
		{ID: firstID, Size: 200, StoredAt: storedAt[firstID], Read: func() ([]byte, error) { reads[firstID]++; return firstContents, nil }},
		{ID: secondID, Size: 100, StoredAt: storedAt[secondID], Read: func() ([]byte, error) { reads[secondID]++; return secondContents, nil }},
	}

	if _, err := writePackSources(directory, sources); err != nil {
		t.Fatalf("writePackSources() error = %v", err)
	}
	indexes, err := loadPackIndexes(directory)
	if err != nil || len(indexes) != 1 {
		t.Fatalf("loadPackIndexes() = %d, %v, want one pack", len(indexes), err)
	}
	if indexes[0].Dictionary != firstID {
		t.Fatalf("dictionary = %s, want largest size-metadata source %s", indexes[0].Dictionary, firstID)
	}
	if reads[firstID] != 1 || reads[secondID] != 1 {
		t.Fatalf("source reads = %v, want each source read once", reads)
	}
	firstOffset := indexes[0].Objects[firstID].Offset
	secondOffset := indexes[0].Objects[secondID].Offset
	if (firstID < secondID && firstOffset >= secondOffset) || (secondID < firstID && secondOffset >= firstOffset) {
		t.Fatalf("pack offsets = %s:%d %s:%d, want lexical ID order", firstID, firstOffset, secondID, secondOffset)
	}
	for _, want := range []struct {
		id       string
		contents []byte
	}{{firstID, firstContents}, {secondID, secondContents}} {
		entry := indexes[0].Objects[want.id]
		if entry.StoredAt != storedAt[want.id] {
			t.Fatalf("index[%s].stored_at = %d, want %d", want.id, entry.StoredAt, storedAt[want.id])
		}
		if entry.RawSize != int64(len(want.contents)) {
			t.Fatalf("index[%s].raw_size = %d, want %d", want.id, entry.RawSize, len(want.contents))
		}
		got, found, err := readFromPacks(directory, indexes, want.id)
		if err != nil || !found || string(got) != string(want.contents) {
			t.Fatalf("readFromPacks(%s) = %q, %v, %v, want original bytes", want.id, got, found, err)
		}
	}
}

func TestPackSourceWriterReadFailureLeavesExistingDataAndNoNewIndex(t *testing.T) {
	directory := t.TempDir()
	existingID, existingContents := testObject("existing packed source")
	existingName, err := writePack(directory, map[string][]byte{existingID: existingContents})
	if err != nil {
		t.Fatalf("writePack(existing) error = %v", err)
	}
	loosePath := filepath.Join(directory, "existing-loose.json")
	if err := os.WriteFile(loosePath, []byte("existing loose"), 0o644); err != nil {
		t.Fatalf("WriteFile(loose) error = %v", err)
	}
	firstID, firstContents := testObject("first streamed source")
	secondID, secondContents := testObject("second streamed source")
	readErr := errors.New("injected second source failure")
	reads := 0
	read := func(contents []byte) func() ([]byte, error) {
		return func() ([]byte, error) {
			reads++
			if reads == 2 {
				return nil, readErr
			}
			return contents, nil
		}
	}
	sources := []packObjectSource{
		{ID: firstID, Size: int64(len(firstContents)) + 1, Read: read(firstContents)},
		{ID: secondID, Size: int64(len(secondContents)), Read: read(secondContents)},
	}

	if _, err := writePackSources(directory, sources); !errors.Is(err, readErr) {
		t.Fatalf("writePackSources() error = %v, want injected read failure", err)
	}
	for _, path := range []string{
		filepath.Join(directory, existingName+".pack"),
		filepath.Join(directory, existingName+".idx.json"),
		loosePath,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("existing data %s missing after source failure: %v", path, err)
		}
	}
	indexes, err := loadPackIndexes(directory)
	if err != nil || len(indexes) != 1 || indexes[0].name != existingName {
		t.Fatalf("loadPackIndexes() = %+v, %v, want only existing index", indexes, err)
	}
}

func TestPackSourceWriterIndexPublishFailureRemovesNewPackAndPreservesExistingData(t *testing.T) {
	directory := t.TempDir()
	existingID, existingContents := testObject("existing data before index failure")
	existingName, err := writePack(directory, map[string][]byte{existingID: existingContents})
	if err != nil {
		t.Fatalf("writePack(existing) error = %v", err)
	}
	loosePath := filepath.Join(directory, "preserved-loose.json")
	if err := os.WriteFile(loosePath, []byte("preserved loose"), 0o644); err != nil {
		t.Fatalf("WriteFile(loose) error = %v", err)
	}
	newID, newContents := testObject("new source before index failure")
	publishErr := errors.New("injected index publish failure")
	indexPath := ""
	removed := make([]string, 0, 2)
	hooks := packWriteHooks{
		writeIndex: func(path string, contents []byte) error {
			indexPath = path
			if err := os.WriteFile(path, contents, 0o600); err != nil {
				return err
			}
			return publishErr
		},
		remove: func(path string) error {
			removed = append(removed, path)
			return os.Remove(path)
		},
	}

	if _, err := writePackSourcesWithHooks(directory, []packObjectSource{{
		ID: newID, Size: int64(len(newContents)), Read: func() ([]byte, error) { return newContents, nil },
	}}, hooks); !errors.Is(err, publishErr) {
		t.Fatalf("writePackSourcesWithHooks() error = %v, want index publish failure", err)
	}
	if len(removed) != 2 || removed[0] != indexPath || filepath.Ext(removed[1]) != ".pack" {
		t.Fatalf("best-effort cleanup paths = %q, want new index then new pack", removed)
	}
	for _, path := range removed {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("new artifact %s stat error = %v, want not exist", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(directory, existingName+".pack"),
		filepath.Join(directory, existingName+".idx.json"),
		loosePath,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("existing data %s missing after index failure: %v", path, err)
		}
	}
}

func TestPackSourceWriterPackWriteAndPublishFailuresPreserveExistingData(t *testing.T) {
	writeErr := errors.New("injected pack write failure")
	publishErr := errors.New("injected pack publish failure")
	tests := []struct {
		name    string
		hooks   packWriteHooks
		wantErr error
	}{
		{
			name: "chunk write",
			hooks: packWriteHooks{writeChunk: func(*os.File, []byte) (int, error) {
				return 0, writeErr
			}},
			wantErr: writeErr,
		},
		{
			name: "pack publish",
			hooks: packWriteHooks{rename: func(string, string) error {
				return publishErr
			}},
			wantErr: publishErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			existingID, existingContents := testObject("existing data before " + tt.name)
			existingName, err := writePack(directory, map[string][]byte{existingID: existingContents})
			if err != nil {
				t.Fatalf("writePack(existing) error = %v", err)
			}
			newID, newContents := testObject("new data during " + tt.name)

			if _, err := writePackSourcesWithHooks(directory, []packObjectSource{{
				ID: newID, Size: int64(len(newContents)), Read: func() ([]byte, error) { return newContents, nil },
			}}, tt.hooks); !errors.Is(err, tt.wantErr) {
				t.Fatalf("writePackSourcesWithHooks() error = %v, want %v", err, tt.wantErr)
			}
			for _, suffix := range []string{".pack", ".idx.json"} {
				if _, err := os.Stat(filepath.Join(directory, existingName+suffix)); err != nil {
					t.Fatalf("existing %s missing after failure: %v", suffix, err)
				}
			}
			indexes, err := loadPackIndexes(directory)
			if err != nil || len(indexes) != 1 || indexes[0].name != existingName {
				t.Fatalf("loadPackIndexes() = %+v, %v, want only existing index", indexes, err)
			}
			temporary, err := filepath.Glob(filepath.Join(directory, ".pack-*.tmp"))
			if err != nil || len(temporary) != 0 {
				t.Fatalf("temporary pack files = %v, %v, want none", temporary, err)
			}
		})
	}
}

func TestRepackCancellationPhasesPreserveOldInputs(t *testing.T) {
	tests := []struct {
		name         string
		completePair bool
		configure    func(context.CancelFunc) packWriteHooks
	}{
		{
			name: "source read",
			configure: func(cancel context.CancelFunc) packWriteHooks {
				return packWriteHooks{afterSourceRead: cancel}
			},
		},
		{
			name: "temp write",
			configure: func(cancel context.CancelFunc) packWriteHooks {
				return packWriteHooks{writeChunk: func(file *os.File, contents []byte) (int, error) {
					written, err := file.Write(contents)
					cancel()
					return written, err
				}}
			},
		},
		{
			name: "pack publish",
			configure: func(cancel context.CancelFunc) packWriteHooks {
				return packWriteHooks{rename: func(oldPath, newPath string) error {
					if err := os.Rename(oldPath, newPath); err != nil {
						return err
					}
					cancel()
					return nil
				}}
			},
		},
		{
			name:         "complete pair",
			completePair: true,
			configure: func(cancel context.CancelFunc) packWriteHooks {
				return packWriteHooks{writeIndex: func(path string, contents []byte) error {
					if err := remote.WriteAtomic(path, contents); err != nil {
						return err
					}
					cancel()
					return nil
				}}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store, err := OpenStorage(t.TempDir(), "")
			if err != nil {
				t.Fatalf("OpenStorage() error = %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			packedID, packedContents := gcObject(t, nil, "old packed input "+tt.name)
			looseID, looseContents := gcObject(t, nil, "old loose input "+tt.name)
			packsDirectory := store.objects.packsDirectory("repo-1")
			oldName, err := writePack(packsDirectory, map[string][]byte{packedID: packedContents})
			if err != nil {
				t.Fatalf("writePack(old) error = %v", err)
			}
			publishForGC(t, store, looseID, looseContents, 48*time.Hour)

			dropped, moved, err := repackRepositoryWithHooks(
				ctx,
				store,
				"repo-1",
				map[string]bool{packedID: true, looseID: true},
				[]string{looseID},
				time.Now().Add(-24*time.Hour),
				repackHooks{packWrite: tt.configure(cancel)},
			)
			if !errors.Is(err, context.Canceled) || dropped != 0 || moved != 0 {
				t.Fatalf("repackRepositoryWithHooks() = %d, %d, %v, want 0, 0, context.Canceled", dropped, moved, err)
			}
			for _, path := range []string{
				filepath.Join(packsDirectory, oldName+".pack"),
				filepath.Join(packsDirectory, oldName+".idx.json"),
				store.objects.loosePath("repo-1", looseID),
			} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("old input %s missing after cancellation: %v", path, err)
				}
			}
			indexes, err := loadPackIndexes(packsDirectory)
			wantIndexes := 1
			if tt.completePair {
				wantIndexes = 2
			}
			if err != nil || len(indexes) != wantIndexes {
				t.Fatalf("loadPackIndexes() = %d, %v, want %d complete indexes", len(indexes), err, wantIndexes)
			}
			packs, err := filepath.Glob(filepath.Join(packsDirectory, "*.pack"))
			if err != nil || len(packs) != wantIndexes {
				t.Fatalf("pack files = %v, %v, want %d", packs, err, wantIndexes)
			}
			temporary, err := filepath.Glob(filepath.Join(packsDirectory, ".pack-*.tmp"))
			if err != nil || len(temporary) != 0 {
				t.Fatalf("temporary pack files = %v, %v, want none", temporary, err)
			}
			if tt.completePair {
				for _, want := range []struct {
					id       string
					contents []byte
				}{{packedID, packedContents}, {looseID, looseContents}} {
					got, found, err := readFromPacks(packsDirectory, indexes, want.id)
					if err != nil || !found || string(got) != string(want.contents) {
						t.Fatalf("readFromPacks(%s) = %q, %v, %v after complete-pair cancellation", want.id, got, found, err)
					}
				}
			}
		})
	}
}

func TestRepackMigratesLegacyRawSizeAndChoosesLargestRawDictionary(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	largeID, largeContents := testObject(strings.Repeat("highly compressible large raw object ", 4096))
	randomContents := make([]byte, 32*1024)
	state := uint32(0x12345678)
	for i := range randomContents {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		randomContents[i] = byte(state)
	}
	smallID, smallContents := testObject(string(randomContents))
	looseID, looseContents := testObject("legacy raw-size rewrite trigger")
	packsDirectory := store.objects.packsDirectory("repo-1")
	oldName, err := writePack(packsDirectory, map[string][]byte{
		largeID: largeContents,
		smallID: smallContents,
	})
	if err != nil {
		t.Fatalf("writePack(old) error = %v", err)
	}
	indexes, err := loadPackIndexes(packsDirectory)
	if err != nil || len(indexes) != 1 {
		t.Fatalf("loadPackIndexes(old) = %d, %v, want one", len(indexes), err)
	}
	if len(largeContents) <= len(smallContents) || indexes[0].Objects[largeID].Length >= indexes[0].Objects[smallID].Length {
		t.Fatalf("fixture raw/compressed ordering not inverted: raw %d/%d compressed %d/%d", len(largeContents), len(smallContents), indexes[0].Objects[largeID].Length, indexes[0].Objects[smallID].Length)
	}
	legacy := indexes[0]
	for id, entry := range legacy.Objects {
		entry.RawSize = 0
		legacy.Objects[id] = entry
	}
	legacyContents, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("json.Marshal(legacy) error = %v", err)
	}
	if strings.Contains(string(legacyContents), "raw_size") {
		t.Fatalf("legacy index contains raw_size: %s", legacyContents)
	}
	if err := os.WriteFile(filepath.Join(packsDirectory, oldName+".idx.json"), legacyContents, 0o600); err != nil {
		t.Fatalf("WriteFile(legacy index) error = %v", err)
	}
	publishForGC(t, store, looseID, looseContents, 48*time.Hour)

	dropped, moved, err := repackRepository(
		ctx,
		store,
		"repo-1",
		map[string]bool{largeID: true, smallID: true, looseID: true},
		[]string{looseID},
		time.Now().Add(-24*time.Hour),
	)
	if err != nil || dropped != 0 || moved != 1 {
		t.Fatalf("repackRepository() = %d, %d, %v, want 0, 1, nil", dropped, moved, err)
	}
	indexes, err = loadPackIndexes(packsDirectory)
	if err != nil || len(indexes) != 1 {
		t.Fatalf("loadPackIndexes(rewritten) = %d, %v, want one", len(indexes), err)
	}
	if indexes[0].Dictionary != largeID {
		t.Fatalf("rewritten dictionary = %s, want largest raw object %s", indexes[0].Dictionary, largeID)
	}
	for id, want := range map[string]int64{
		largeID: int64(len(largeContents)),
		smallID: int64(len(smallContents)),
		looseID: int64(len(looseContents)),
	} {
		if got := indexes[0].Objects[id].RawSize; got != want {
			t.Fatalf("rewritten index[%s].raw_size = %d, want %d", id, got, want)
		}
	}
}

func TestPackRoundTripsObjectBytes(t *testing.T) {
	directory := t.TempDir()
	firstID, firstContents := testObject("first packed object")
	secondID, secondContents := testObject("second packed object")

	packName, err := writePack(directory, map[string][]byte{firstID: firstContents, secondID: secondContents})
	if err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, packName+".pack")); err != nil {
		t.Fatalf("pack file missing: %v", err)
	}

	indexes, err := loadPackIndexes(directory)
	if err != nil || len(indexes) != 1 {
		t.Fatalf("loadPackIndexes() = %d, %v, want one pack", len(indexes), err)
	}
	for _, want := range []struct {
		id       string
		contents []byte
	}{{firstID, firstContents}, {secondID, secondContents}} {
		got, found, err := readFromPacks(directory, indexes, want.id)
		if err != nil || !found || string(got) != string(want.contents) {
			t.Fatalf("readFromPacks(%s) = %q, %v, %v, want original bytes", want.id, got, found, err)
		}
	}
}

func TestFileStorageServesPackedObjectsTransparently(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	packedID, packedContents := testObject("packed only object")
	looseID, looseContents := testObject("loose object")

	if _, err := writePack(filepath.Join(store.objects.root, "repo-1", "packs"), map[string][]byte{packedID: packedContents}); err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	if _, err := store.PublishObject(ctx, "repo-1", looseID, looseContents); err != nil {
		t.Fatalf("PublishObject() error = %v", err)
	}

	got, err := store.ReadObject(ctx, "repo-1", packedID)
	if err != nil || string(got) != string(packedContents) {
		t.Fatalf("ReadObject(packed) = %q, %v, want packed contents", got, err)
	}
	ids, err := store.ListObjects(ctx, "repo-1")
	if err != nil || len(ids) != 2 {
		t.Fatalf("ListObjects() = %v, %v, want loose and packed union", ids, err)
	}

	if _, err := store.PublishObject(ctx, "repo-1", packedID, packedContents); err != nil {
		t.Fatalf("PublishObject(duplicate of packed) error = %v", err)
	}
	ids, err = store.ListObjects(ctx, "repo-1")
	if err != nil || len(ids) != 2 {
		t.Fatalf("ListObjects(after duplicate) = %v, %v, want no duplicate ids", ids, err)
	}
	if got, err := store.ReadObject(ctx, "repo-1", packedID); err != nil || string(got) != string(packedContents) {
		t.Fatalf("ReadObject(loose shadowing pack) = %q, %v", got, err)
	}
}

func TestPackCatalogCacheReusesSnapshotForUnchangedFingerprint(t *testing.T) {
	directory := t.TempDir()
	objectID, contents := testObject("catalog cache object")
	if _, err := writePack(directory, map[string][]byte{objectID: contents}); err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	cache := newPackCatalogCache()
	readFile := cache.readFile
	loads := 0
	cache.readFile = func(path string) ([]byte, error) {
		loads++
		return readFile(path)
	}

	first, err := cache.load("repo-1", directory)
	if err != nil {
		t.Fatalf("cache.load(first) error = %v", err)
	}
	second, err := cache.load("repo-1", directory)
	if err != nil {
		t.Fatalf("cache.load(second) error = %v", err)
	}
	if first != second {
		t.Fatal("cache.load(second) returned a different snapshot for the same fingerprint")
	}
	if loads != 1 {
		t.Fatalf("index content loads = %d, want 1", loads)
	}
}

func TestPackCatalogCacheRefreshesWhenExternalIndexChanges(t *testing.T) {
	directory := t.TempDir()
	firstID, firstContents := testObject("first external pack")
	if _, err := writePack(directory, map[string][]byte{firstID: firstContents}); err != nil {
		t.Fatalf("writePack(first) error = %v", err)
	}
	cache := newPackCatalogCache()
	first, err := cache.load("repo-1", directory)
	if err != nil {
		t.Fatalf("cache.load(first) error = %v", err)
	}

	secondID, secondContents := testObject("second external pack")
	if _, err := writePack(directory, map[string][]byte{secondID: secondContents}); err != nil {
		t.Fatalf("writePack(second) error = %v", err)
	}
	second, err := cache.load("repo-1", directory)
	if err != nil {
		t.Fatalf("cache.load(second) error = %v", err)
	}
	if first == second {
		t.Fatal("cache.load(second) reused the stale snapshot after an external index change")
	}
	session := second.newReadSession()
	got, found, err := session.readObject(secondID)
	session.Close()
	if err != nil || !found || string(got) != string(secondContents) {
		t.Fatalf("session.readObject(new object) = %q, %v, %v, want new contents", got, found, err)
	}
}

func TestPackCatalogCacheReadErrorDoesNotPublishPartialCatalog(t *testing.T) {
	directory := t.TempDir()
	objectID, contents := testObject("retry index read")
	if _, err := writePack(directory, map[string][]byte{objectID: contents}); err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	cache := newPackCatalogCache()
	readFile := cache.readFile
	readErr := errors.New("injected index read failure")
	var reads atomic.Int32
	cache.readFile = func(path string) ([]byte, error) {
		if reads.Add(1) == 1 {
			return nil, readErr
		}
		return readFile(path)
	}

	if _, err := cache.load("repo-1", directory); !errors.Is(err, readErr) || domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("cache.load(first) error = %v, want typed injected read failure", err)
	}
	cache.mu.Lock()
	_, cached := cache.entries["repo-1"]
	cache.mu.Unlock()
	if cached {
		t.Fatal("cache.load(first) published a partial catalog after an index read failure")
	}
	catalog, err := cache.load("repo-1", directory)
	if err != nil {
		t.Fatalf("cache.load(second) error = %v", err)
	}
	if reads.Load() != 2 || len(catalog.indexes) != 1 {
		t.Fatalf("cache.load retry = %d reads, %d indexes; want 2 reads and 1 index", reads.Load(), len(catalog.indexes))
	}
}

func TestPackCatalogCacheInvalidateDuringLoadForcesRetry(t *testing.T) {
	directory := t.TempDir()
	objectID, contents := testObject("invalidate blocked catalog load")
	if _, err := writePack(directory, map[string][]byte{objectID: contents}); err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	cache := newPackCatalogCache()
	readFile := cache.readFile
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	cache.readFile = func(path string) ([]byte, error) {
		if reads.Add(1) == 1 {
			close(started)
			<-release
		}
		return readFile(path)
	}
	type loadResult struct {
		catalog *packCatalog
		err     error
	}
	result := make(chan loadResult, 1)
	go func() {
		catalog, err := cache.load("repo-1", directory)
		result <- loadResult{catalog: catalog, err: err}
	}()
	<-started
	cache.invalidate("repo-1")
	close(release)
	loaded := <-result
	if loaded.err != nil {
		t.Fatalf("cache.load() error = %v", loaded.err)
	}
	if reads.Load() != 2 {
		t.Fatalf("index content reads = %d, want 2 after invalidate forced a retry", reads.Load())
	}
	cache.mu.Lock()
	cached := cache.entries["repo-1"]
	cache.mu.Unlock()
	if cached != loaded.catalog {
		t.Fatal("cache.load() did not publish only the post-invalidation retry snapshot")
	}
}

func TestPackCatalogCacheFingerprintChangeDuringLoadForcesRetry(t *testing.T) {
	directory := t.TempDir()
	firstID, firstContents := testObject("blocked external first pack")
	if _, err := writePack(directory, map[string][]byte{firstID: firstContents}); err != nil {
		t.Fatalf("writePack(first) error = %v", err)
	}
	cache := newPackCatalogCache()
	readFile := cache.readFile
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	cache.readFile = func(path string) ([]byte, error) {
		if reads.Add(1) == 1 {
			close(started)
			<-release
		}
		return readFile(path)
	}
	type loadResult struct {
		catalog *packCatalog
		err     error
	}
	result := make(chan loadResult, 1)
	go func() {
		catalog, err := cache.load("repo-1", directory)
		result <- loadResult{catalog: catalog, err: err}
	}()
	<-started
	secondID, secondContents := testObject("blocked external second pack")
	if _, err := writePack(directory, map[string][]byte{secondID: secondContents}); err != nil {
		t.Fatalf("writePack(second) error = %v", err)
	}
	close(release)
	loaded := <-result
	if loaded.err != nil {
		t.Fatalf("cache.load() error = %v", loaded.err)
	}
	if reads.Load() != 3 {
		t.Fatalf("index content reads = %d, want 3 across stale load and two-index retry", reads.Load())
	}
	session := loaded.catalog.newReadSession()
	got, found, err := session.readObject(secondID)
	session.Close()
	if err != nil || !found || string(got) != string(secondContents) {
		t.Fatalf("session.readObject(new object) = %q, %v, %v, want externally added contents", got, found, err)
	}
}

func TestPackCatalogCacheIndexRemovalDuringReadRetriesChangedFingerprint(t *testing.T) {
	directory := t.TempDir()
	objectID, contents := testObject("removed during blocked index read")
	packName, err := writePack(directory, map[string][]byte{objectID: contents})
	if err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	cache := newPackCatalogCache()
	readFile := cache.readFile
	started := make(chan struct{})
	release := make(chan struct{})
	cache.readFile = func(path string) ([]byte, error) {
		close(started)
		<-release
		return readFile(path)
	}
	type loadResult struct {
		catalog *packCatalog
		err     error
	}
	result := make(chan loadResult, 1)
	go func() {
		catalog, err := cache.load("repo-1", directory)
		result <- loadResult{catalog: catalog, err: err}
	}()
	<-started
	if err := os.Remove(filepath.Join(directory, packName+".idx.json")); err != nil {
		t.Fatalf("Remove(index) error = %v", err)
	}
	close(release)
	loaded := <-result
	if loaded.err != nil {
		t.Fatalf("cache.load() error = %v, want retry after fingerprint-changing removal", loaded.err)
	}
	if len(loaded.catalog.indexes) != 0 || len(loaded.catalog.fingerprint) != 0 {
		t.Fatalf("cache.load() = %d indexes, %d fingerprint entries; want refreshed empty catalog", len(loaded.catalog.indexes), len(loaded.catalog.fingerprint))
	}
}

func TestRepackInvalidatesPackCatalogCache(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	packedID, packedContents := gcObject(t, nil, "cached packed object")
	if _, err := writePack(store.objects.packsDirectory("repo-1"), map[string][]byte{packedID: packedContents}); err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	if _, err := store.objects.loadPackCatalog("repo-1"); err != nil {
		t.Fatalf("loadPackCatalog() error = %v", err)
	}
	looseID, looseContents := gcObject(t, nil, "repack invalidation trigger")
	publishForGC(t, store, looseID, looseContents, 48*time.Hour)
	if _, _, err := repackRepository(ctx, store, "repo-1", map[string]bool{packedID: true, looseID: true}, []string{looseID}, time.Now().Add(-24*time.Hour)); err != nil {
		t.Fatalf("repackRepository() error = %v", err)
	}
	store.objects.packCatalogCache.mu.Lock()
	_, cached := store.objects.packCatalogCache.entries["repo-1"]
	store.objects.packCatalogCache.mu.Unlock()
	if cached {
		t.Fatal("repackRepository() left the repository catalog cached after publish")
	}
}

func TestPackReadSessionPreparesDictionaryAndDecodersOnce(t *testing.T) {
	directory := t.TempDir()
	objects := make(map[string][]byte, 3)
	for _, contents := range [][]byte{
		[]byte(strings.Repeat("dictionary source ", 128)),
		[]byte(strings.Repeat("dictionary source ", 96) + "first"),
		[]byte(strings.Repeat("dictionary source ", 80) + "second"),
	} {
		digest := blake3.Sum256(contents)
		objects[fmt.Sprintf("%x", digest[:])] = contents
	}
	if _, err := writePack(directory, objects); err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	catalog, err := newPackCatalogCache().load("repo-1", directory)
	if err != nil {
		t.Fatalf("cache.load() error = %v", err)
	}
	if len(catalog.indexes) != 1 || catalog.indexes[0].Dictionary == "" {
		t.Fatalf("catalog indexes = %+v, want one dictionary pack", catalog.indexes)
	}
	session := catalog.newReadSession()
	dictionaryEntry := catalog.indexes[0].Objects[catalog.indexes[0].Dictionary]
	dictionaryReads := 0
	readRange := session.ops.readRange
	session.ops.readRange = func(directory, packName string, entry packEntry) ([]byte, error) {
		if entry == dictionaryEntry {
			dictionaryReads++
		}
		return readRange(directory, packName, entry)
	}
	plainCreates, dictionaryCreates, closes := 0, 0, 0
	newPlainDecoder := session.ops.newPlainDecoder
	session.ops.newPlainDecoder = func() (packDecoder, error) {
		plainCreates++
		decoder, err := newPlainDecoder()
		return countingPackDecoder{packDecoder: decoder, closes: &closes}, err
	}
	newDictionaryDecoder := session.ops.newDictionaryDecoder
	session.ops.newDictionaryDecoder = func(dictionary []byte) (packDecoder, error) {
		dictionaryCreates++
		decoder, err := newDictionaryDecoder(dictionary)
		return countingPackDecoder{packDecoder: decoder, closes: &closes}, err
	}
	for id, want := range objects {
		if id == catalog.indexes[0].Dictionary {
			continue
		}
		got, found, err := session.readObject(id)
		if err != nil || !found || string(got) != string(want) {
			t.Fatalf("session.readObject(%s) = %q, %v, %v, want original bytes", id, got, found, err)
		}
	}
	session.Close()
	if dictionaryReads != 1 || plainCreates != 1 || dictionaryCreates != 1 || closes != 2 {
		t.Fatalf("session preparation = dictionary reads %d, plain decoders %d, dictionary decoders %d, closes %d; want 1, 1, 1, 2", dictionaryReads, plainCreates, dictionaryCreates, closes)
	}
}

func TestRepackMovesAgedReachableLooseIntoPack(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rootID, rootContents := gcObject(t, nil, "repack root")
	tipID, tipContents := gcObject(t, []string{rootID}, "repack tip")
	freshID, freshContents := gcObject(t, nil, "fresh loose stays")
	publishForGC(t, store, rootID, rootContents, 48*time.Hour)
	publishForGC(t, store, tipID, tipContents, 48*time.Hour)
	publishForGC(t, store, freshID, freshContents, 0)
	setTip(t, store, tipID)

	result, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	repo := result.Repositories["repo-1"]
	if repo.Packed != 2 || repo.Deleted != 0 || repo.Aborted {
		t.Fatalf("RunGC(repack) = %+v, want 2 packed, 0 deleted", repo)
	}
	loose, err := store.objects.listLooseObjects(ctx, "repo-1")
	if err != nil || len(loose) != 1 || loose[0] != freshID {
		t.Fatalf("loose after repack = %v, %v, want only the fresh object", loose, err)
	}
	for _, want := range []struct {
		id       string
		contents []byte
	}{{rootID, rootContents}, {tipID, tipContents}, {freshID, freshContents}} {
		got, err := store.ReadObject(ctx, "repo-1", want.id)
		if err != nil || string(got) != string(want.contents) {
			t.Fatalf("ReadObject(%s) after repack = %q, %v", want.id, got, err)
		}
	}

	again, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil || again.Repositories["repo-1"].Packed != 0 || again.Repositories["repo-1"].Deleted != 0 {
		t.Fatalf("RunGC(second) = %+v, %v, want no-op", again.Repositories["repo-1"], err)
	}
}

func TestRepackRewriteDropsUnreachablePackedEntries(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tipID, tipContents := gcObject(t, nil, "packed tip")
	orphanID, orphanContents := gcObject(t, nil, "packed orphan")
	packsDirectory := filepath.Join(store.objects.root, "repo-1", "packs")
	past := time.Now().Add(-48 * time.Hour)
	_, err = writePack(
		packsDirectory,
		map[string][]byte{tipID: tipContents, orphanID: orphanContents},
		map[string]int64{tipID: past.Unix(), orphanID: past.Unix()},
	)
	if err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	setTip(t, store, tipID)

	result, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	repo := result.Repositories["repo-1"]
	if repo.Deleted != 1 || repo.Aborted {
		t.Fatalf("RunGC(rewrite) = %+v, want the packed orphan dropped", repo)
	}
	if _, err := store.ReadObject(ctx, "repo-1", orphanID); !isMissingObjectError(err) {
		t.Fatalf("packed orphan still readable after rewrite: %v", err)
	}
	if got, err := store.ReadObject(ctx, "repo-1", tipID); err != nil || string(got) != string(tipContents) {
		t.Fatalf("packed tip lost in rewrite: %q, %v", got, err)
	}
	ids, err := store.ListObjects(ctx, "repo-1")
	if err != nil || len(ids) != 1 || ids[0] != tipID {
		t.Fatalf("ListObjects after rewrite = %v, %v, want only the tip", ids, err)
	}
}

func TestRepackCollectsDropMoveAndSourcesWithOneStatPerInputPack(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sharedID, sharedContents := gcObject(t, nil, "shared packed entry")
	shadowID, shadowContents := gcObject(t, nil, "newer fresh entry shadows aged duplicate")
	orphanID, orphanContents := gcObject(t, nil, "aged packed orphan")
	looseID, looseContents := gcObject(t, nil, "aged loose move")
	storedAt := time.Now().Add(-48 * time.Hour).Unix()
	packsDirectory := store.objects.packsDirectory("repo-1")
	firstName, err := writePack(
		packsDirectory,
		map[string][]byte{sharedID: sharedContents, shadowID: shadowContents, orphanID: orphanContents},
		map[string]int64{sharedID: storedAt, shadowID: storedAt, orphanID: storedAt},
	)
	if err != nil {
		t.Fatalf("writePack(first) error = %v", err)
	}
	secondName, err := writePack(
		packsDirectory,
		map[string][]byte{sharedID: sharedContents, shadowID: shadowContents},
		map[string]int64{sharedID: storedAt, shadowID: time.Now().Unix()},
	)
	if err != nil {
		t.Fatalf("writePack(second) error = %v", err)
	}
	publishForGC(t, store, looseID, looseContents, 48*time.Hour)
	packStats := make(map[string]int)
	hooks := repackHooks{stat: func(path string) (os.FileInfo, error) {
		if filepath.Ext(path) == ".pack" {
			packStats[path]++
		}
		return os.Stat(path)
	}}

	dropped, moved, err := repackRepositoryWithHooks(
		ctx,
		store,
		"repo-1",
		map[string]bool{sharedID: true, looseID: true},
		[]string{looseID},
		time.Now().Add(-24*time.Hour),
		hooks,
	)
	if err != nil || dropped != 1 || moved != 1 {
		t.Fatalf("repackRepositoryWithHooks() = %d, %d, %v, want 1 drop and 1 move", dropped, moved, err)
	}
	for _, name := range []string{firstName, secondName} {
		path := filepath.Join(packsDirectory, name+".pack")
		if packStats[path] != 1 {
			t.Fatalf("Stat(%s) calls = %d, want one catalog-pass stat", path, packStats[path])
		}
	}
	indexes, err := loadPackIndexes(packsDirectory)
	if err != nil || len(indexes) != 1 {
		t.Fatalf("loadPackIndexes() = %d, %v, want one rewritten pack", len(indexes), err)
	}
	for _, want := range []struct {
		id       string
		contents []byte
	}{{sharedID, sharedContents}, {shadowID, shadowContents}, {looseID, looseContents}} {
		got, found, err := readFromPacks(packsDirectory, indexes, want.id)
		if err != nil || !found || string(got) != string(want.contents) {
			t.Fatalf("readFromPacks(%s) = %q, %v, %v after rewrite", want.id, got, found, err)
		}
	}
	if _, found, err := readFromPacks(packsDirectory, indexes, orphanID); err != nil || found {
		t.Fatalf("readFromPacks(orphan) = %v, %v, want dropped", found, err)
	}
}

func TestRepackPreservesStoredAtAcrossRewritesUntilOriginalGraceExpires(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tipID, tipContents := gcObject(t, nil, "stored-at tip")
	orphanID, orphanContents := gcObject(t, nil, "stored-at orphan")
	firstLooseID, firstLooseContents := gcObject(t, nil, "first rewrite trigger")
	secondLooseID, secondLooseContents := gcObject(t, nil, "second rewrite trigger")
	storedAt := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	packsDirectory := store.objects.packsDirectory("repo-1")
	if _, err := writePack(
		packsDirectory,
		map[string][]byte{tipID: tipContents, orphanID: orphanContents},
		map[string]int64{tipID: storedAt.Unix(), orphanID: storedAt.Unix()},
	); err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	publishForGC(t, store, firstLooseID, firstLooseContents, 48*time.Hour)
	publishForGC(t, store, secondLooseID, secondLooseContents, 48*time.Hour)
	reachable := map[string]bool{tipID: true, firstLooseID: true, secondLooseID: true}
	beforeGrace := storedAt.Add(-time.Second)

	assertStoredAt := func(want int64) {
		t.Helper()
		indexes, err := loadPackIndexes(packsDirectory)
		if err != nil || len(indexes) != 1 {
			t.Fatalf("loadPackIndexes() = %d, %v, want one pack", len(indexes), err)
		}
		entry, ok := indexes[0].Objects[orphanID]
		if !ok || entry.StoredAt != want {
			t.Fatalf("orphan pack entry = %+v, %v, want stored_at %d", entry, ok, want)
		}
	}

	if dropped, moved, err := repackRepository(ctx, store, "repo-1", reachable, []string{firstLooseID}, beforeGrace); err != nil || dropped != 0 || moved != 1 {
		t.Fatalf("repackRepository(first rewrite) = %d, %d, %v, want 0, 1, nil", dropped, moved, err)
	}
	assertStoredAt(storedAt.Unix())
	if dropped, moved, err := repackRepository(ctx, store, "repo-1", reachable, []string{secondLooseID}, beforeGrace); err != nil || dropped != 0 || moved != 1 {
		t.Fatalf("repackRepository(second rewrite) = %d, %d, %v, want 0, 1, nil", dropped, moved, err)
	}
	assertStoredAt(storedAt.Unix())

	if dropped, moved, err := repackRepository(ctx, store, "repo-1", reachable, nil, storedAt); err != nil || dropped != 1 || moved != 0 {
		t.Fatalf("repackRepository(after original grace) = %d, %d, %v, want 1, 0, nil", dropped, moved, err)
	}
	if _, err := store.ReadObject(ctx, "repo-1", orphanID); !isMissingObjectError(err) {
		t.Fatalf("orphan remains after its original grace expired: %v", err)
	}
}

func TestRepackFallsBackToPackMtimeWhenStoredAtIsMissing(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	orphanID, orphanContents := gcObject(t, nil, "legacy index orphan")
	packsDirectory := store.objects.packsDirectory("repo-1")
	packName, err := writePack(packsDirectory, map[string][]byte{orphanID: orphanContents})
	if err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	indexPath := filepath.Join(packsDirectory, packName+".idx.json")
	indexes, err := loadPackIndexes(packsDirectory)
	if err != nil || len(indexes) != 1 {
		t.Fatalf("loadPackIndexes() = %d, %v, want one pack", len(indexes), err)
	}
	entry := indexes[0].Objects[orphanID]
	entry.StoredAt = 0
	indexes[0].Objects[orphanID] = entry
	legacyIndex, err := json.Marshal(indexes[0])
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(legacyIndex), "stored_at") {
		t.Fatalf("legacy index contains stored_at: %s", legacyIndex)
	}
	if err := os.WriteFile(indexPath, legacyIndex, 0o644); err != nil {
		t.Fatalf("WriteFile(legacy index) error = %v", err)
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	if dropped, moved, err := repackRepository(ctx, store, "repo-1", nil, nil, cutoff); err != nil || dropped != 0 || moved != 0 {
		t.Fatalf("repackRepository(fresh legacy pack) = %d, %d, %v, want no-op", dropped, moved, err)
	}
	past := cutoff.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(packsDirectory, packName+".pack"), past, past); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	if dropped, moved, err := repackRepository(ctx, store, "repo-1", nil, nil, cutoff); err != nil || dropped != 1 || moved != 0 {
		t.Fatalf("repackRepository(aged legacy pack) = %d, %d, %v, want 1, 0, nil", dropped, moved, err)
	}
}

func TestRepackAbortsWhenReachablePackedEntryCannotBeRead(t *testing.T) {
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rootID, rootContents := gcObject(t, nil, "packed abort root")
	tipID, tipContents := gcObject(t, []string{rootID}, "packed abort tip")
	packsDirectory := store.objects.packsDirectory("repo-1")
	packName, err := writePack(packsDirectory, map[string][]byte{rootID: rootContents, tipID: tipContents})
	if err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	for id, contents := range map[string][]byte{rootID: rootContents, tipID: tipContents} {
		publishForGC(t, store, id, contents, 48*time.Hour)
	}
	setTip(t, store, tipID)
	packPath := filepath.Join(packsDirectory, packName+".pack")
	if err := os.Truncate(packPath, 0); err != nil {
		t.Fatalf("Truncate(pack) error = %v", err)
	}

	result, err := RunGC(ctx, store, []string{"repo-1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("RunGC() error = %v", err)
	}
	repo := result.Repositories["repo-1"]
	if !repo.Aborted || repo.Deleted != 0 || repo.Packed != 0 {
		t.Fatalf("RunGC(unreadable reachable pack) = %+v, want aborted with no repack mutation", repo)
	}
	if _, err := os.Stat(packPath); err != nil {
		t.Fatalf("original pack removed after aborted repack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(packsDirectory, packName+".idx.json")); err != nil {
		t.Fatalf("original pack index removed after aborted repack: %v", err)
	}
	entries, err := os.ReadDir(packsDirectory)
	if err != nil || len(entries) != 2 {
		t.Fatalf("packs after aborted repack = %d entries, %v, want original pack and index only", len(entries), err)
	}
	for id, contents := range map[string][]byte{rootID: rootContents, tipID: tipContents} {
		got, err := store.ReadObject(ctx, "repo-1", id)
		if err != nil || string(got) != string(contents) {
			t.Fatalf("ReadObject(%s) after aborted repack = %q, %v", id, got, err)
		}
	}
}

func TestRepackAbortsWhenAgedReachableLooseCannotBeRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix owner permission enforcement")
	}
	ctx := t.Context()
	store, err := OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	id, contents := gcObject(t, nil, "unreadable reachable loose")
	publishForGC(t, store, id, contents, 48*time.Hour)
	path := store.objects.loosePath("repo-1", id)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("Chmod(0) error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	dropped, moved, err := repackRepository(ctx, store, "repo-1", map[string]bool{id: true}, []string{id}, time.Now().Add(-24*time.Hour))
	if err == nil || dropped != 0 || moved != 0 {
		t.Fatalf("repackRepository(unreadable reachable loose) = %d, %d, %v, want abort with zero mutations", dropped, moved, err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("reachable loose removed after aborted repack: %v", err)
	}
}

func TestPackDictionaryAbsorbsCrossSnapshotRedundancy(t *testing.T) {
	directory := t.TempDir()
	sharedBody := strings.Repeat(`{"entity":"payment.Authorize","note":"retries must stay idempotent across gateway failures"},`, 400)
	objects := make(map[string][]byte, 5)
	rawTotal := 0
	for i := range 5 {
		contents := fmt.Appendf(nil, `{"schema_version":3,"revision":%d,"entities":[%s"end-%d"]}`, i, sharedBody, i)
		digest := blake3.Sum256(contents)
		objects[fmt.Sprintf("%x", digest[:])] = contents
		rawTotal += len(contents)
	}
	packName, err := writePack(directory, objects)
	if err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	indexes, err := loadPackIndexes(directory)
	if err != nil || len(indexes) != 1 || indexes[0].Dictionary == "" {
		t.Fatalf("loadPackIndexes() = %+v, %v, want v2 index with dictionary", indexes, err)
	}
	info, err := os.Stat(filepath.Join(directory, packName+".pack"))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	singleCompressed := indexes[0].Objects[indexes[0].Dictionary].Length
	if info.Size() > 2*singleCompressed {
		t.Fatalf("pack size %d exceeds 2x one compressed snapshot (%d) — cross-snapshot redundancy not absorbed", info.Size(), singleCompressed)
	}
	for id, contents := range objects {
		got, found, err := readFromPacks(directory, indexes, id)
		if err != nil || !found || string(got) != string(contents) {
			t.Fatalf("readFromPacks(%s) = %v, %v, want original bytes", id, found, err)
		}
	}
}

func TestPackMissAndCorruptionAreDetected(t *testing.T) {
	directory := t.TempDir()
	storedID, storedContents := testObject("stored entry")
	missingID, _ := testObject("never packed")
	packName, err := writePack(directory, map[string][]byte{storedID: storedContents})
	if err != nil {
		t.Fatalf("writePack() error = %v", err)
	}
	indexes, err := loadPackIndexes(directory)
	if err != nil {
		t.Fatalf("loadPackIndexes() error = %v", err)
	}

	if _, found, err := readFromPacks(directory, indexes, missingID); found || err != nil {
		t.Fatalf("readFromPacks(missing) = %v, %v, want clean miss", found, err)
	}

	packPath := filepath.Join(directory, packName+".pack")
	contents, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for i := range contents {
		contents[i] ^= 0xff
	}
	if err := os.WriteFile(packPath, contents, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, _, err := readFromPacks(directory, indexes, storedID); err == nil {
		t.Fatalf("readFromPacks(corrupted) expected an error")
	}
}

func BenchmarkFileStorageReadPackedObjectWarm(b *testing.B) {
	ctx := b.Context()
	store, err := OpenStorage(b.TempDir(), "")
	if err != nil {
		b.Fatalf("OpenStorage() error = %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	objectID, contents := testObject(strings.Repeat("warm packed object ", 256))
	if _, err := writePack(store.objects.packsDirectory("repo-1"), map[string][]byte{objectID: contents}); err != nil {
		b.Fatalf("writePack() error = %v", err)
	}
	if _, err := store.ReadObject(ctx, "repo-1", objectID); err != nil {
		b.Fatalf("ReadObject(warmup) error = %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := store.ReadObject(ctx, "repo-1", objectID); err != nil {
			b.Fatalf("ReadObject() error = %v", err)
		}
	}
}

func BenchmarkWritePack(b *testing.B) {
	directory := b.TempDir()
	sources := make([]packObjectSource, 0, 32)
	totalBytes := int64(0)
	for i := range 32 {
		id, contents := testObject(strings.Repeat(fmt.Sprintf("benchmark object %02d ", i), 512))
		sources = append(sources, packObjectSource{
			ID:   id,
			Size: int64(len(contents)),
			Read: func() ([]byte, error) { return contents, nil },
		})
		totalBytes += int64(len(contents))
	}
	b.ReportAllocs()
	b.SetBytes(totalBytes)
	b.ResetTimer()
	for range b.N {
		name, err := writePackSources(directory, sources)
		if err != nil {
			b.Fatalf("writePackSources() error = %v", err)
		}
		b.StopTimer()
		if err := os.Remove(filepath.Join(directory, name+".pack")); err != nil {
			b.Fatalf("Remove(pack) error = %v", err)
		}
		if err := os.Remove(filepath.Join(directory, name+".idx.json")); err != nil {
			b.Fatalf("Remove(index) error = %v", err)
		}
		b.StartTimer()
	}
}
