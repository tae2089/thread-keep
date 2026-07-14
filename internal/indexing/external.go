package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

const protocolVersion = 1

const (
	maxPackStdout = 8 << 20
	maxPackStderr = 64 << 10
)

const (
	bundledPackDirectoryEnv = "THREAD_KEEP_BUNDLED_PACK_DIR"
	bundledPackVersionEnv   = "THREAD_KEEP_BUNDLED_PACK_VERSION"
)

type ProcessIndexer struct {
	language   Language
	path       string
	descriptor Descriptor
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

type protocolRequest struct {
	ProtocolVersion int      `json:"protocol_version"`
	RepositoryRoot  string   `json:"repository_root"`
	SourceSHA       string   `json:"source_sha"`
	Language        Language `json:"language"`
	Files           []string `json:"files"`
}

type protocolResponse struct {
	ProtocolVersion int          `json:"protocol_version"`
	Indexer         Descriptor   `json:"indexer"`
	Language        Language     `json:"language"`
	Entities        []packEntity `json:"entities"`
	Diagnostics     []string     `json:"diagnostics"`
}

type packEntity struct {
	Path           string            `json:"path"`
	Kind           domain.EntityKind `json:"kind"`
	Name           string            `json:"name"`
	QualifiedName  string            `json:"qualified_name"`
	Signature      string            `json:"signature"`
	StartLine      int               `json:"start_line"`
	EndLine        int               `json:"end_line"`
	StructuralHash string            `json:"structural_hash"`
}

func List(ctx context.Context, root string) ([]domain.IndexerStatus, error) {
	candidates, err := DetectContext(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("detect indexer languages: %w", err)
	}
	detected := make(map[Language]bool, len(candidates))
	for _, candidate := range candidates {
		detected[candidate.Language] = true
	}
	statuses := []domain.IndexerStatus{{Language: string(Go), PackID: GoIndexer{}.Descriptor().ID, State: domain.IndexerBuiltin, Detected: detected[Go]}}
	for _, language := range externalPackLanguages {
		pack, found, err := findInstalledPack(language)
		if err != nil {
			return nil, err
		}
		status := domain.IndexerStatus{Language: string(language), PackID: packID(language), State: domain.IndexerMissing, Detected: detected[language]}
		if found {
			status.State = domain.IndexerInstalled
			status.Path = pack.Path
			status.Version = pack.Descriptor.Version
			status.SHA256 = pack.SHA256
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func FindInstalledPack(language Language) (Indexer, bool) {
	pack, found, err := findInstalledPack(language)
	if err != nil || !found {
		return nil, false
	}
	return ProcessIndexer{language: language, path: pack.Path, descriptor: pack.Descriptor}, true
}

func findInstalledPack(language Language) (installedPack, bool, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return installedPack{}, false, fmt.Errorf("locate user configuration directory: %w", err)
	}
	return resolveAvailablePack(configDir, os.Getenv(bundledPackDirectoryEnv), os.Getenv(bundledPackVersionEnv), language)
}

func (p ProcessIndexer) Descriptor() Descriptor {
	if p.descriptor.ID != "" {
		return p.descriptor
	}
	return Descriptor{ID: packID(p.language)}
}

func (p ProcessIndexer) Index(ctx context.Context, request Request) (Result, error) {
	payload, err := json.Marshal(protocolRequest{ProtocolVersion: protocolVersion, RepositoryRoot: request.RepositoryRoot, SourceSHA: request.SourceSHA, Language: request.Language, Files: request.Files})
	if err != nil {
		return Result{}, fmt.Errorf("encode pack request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, p.path, "index", "--protocol-version=1")
	command.Stdin = bytes.NewReader(append(payload, '\n'))
	var stdout, stderr boundedBuffer
	stdout.limit = maxPackStdout
	stderr.limit = maxPackStderr
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	if stdout.exceeded {
		return Result{}, errors.New("pack response exceeds 8 MiB")
	}
	if err != nil {
		if stderr.Len() > 0 {
			return Result{}, fmt.Errorf("run %s: %s", p.Descriptor().ID, strings.TrimSpace(stderr.String()))
		}
		return Result{}, fmt.Errorf("run %s: %w", p.Descriptor().ID, err)
	}
	response, err := decodeResponse(stdout.Bytes())
	if err != nil {
		return Result{}, err
	}
	if response.ProtocolVersion != protocolVersion || response.Language != request.Language {
		return Result{}, errors.New("pack response has an incompatible protocol or language")
	}
	expected := p.Descriptor()
	if response.Indexer.ID != expected.ID || response.Indexer.Version == "" || expected.Version != "" && response.Indexer.Version != expected.Version {
		return Result{}, errors.New("pack response has an invalid indexer identity")
	}
	entities, err := normalizeEntities(request, response.Entities)
	if err != nil {
		return Result{}, err
	}
	return Result{Indexer: response.Indexer, Entities: entities, Diagnostics: response.Diagnostics}, nil
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return len(value), nil
	}
	if len(value) > remaining {
		_, _ = b.Buffer.Write(value[:remaining])
		b.exceeded = true
		return len(value), nil
	}
	return b.Buffer.Write(value)
}

func decodeResponse(output []byte) (protocolResponse, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	var response protocolResponse
	if err := decoder.Decode(&response); err != nil {
		return protocolResponse{}, fmt.Errorf("decode pack response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return protocolResponse{}, errors.New("pack response contains more than one JSON value")
	}
	return response, nil
}

func normalizeEntities(request Request, raw []packEntity) ([]domain.Entity, error) {
	allowed := make(map[string]struct{}, len(request.Files))
	for _, file := range request.Files {
		allowed[file] = struct{}{}
	}
	entities := make([]domain.Entity, 0, len(raw))
	keys := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		if _, ok := allowed[item.Path]; !ok || !validRelativePath(item.Path) {
			return nil, fmt.Errorf("pack returned an unrequested path %q", item.Path)
		}
		if item.Name == "" || item.QualifiedName == "" || item.StartLine <= 0 || item.EndLine < item.StartLine || item.StructuralHash == "" || !validKind(item.Kind) {
			return nil, errors.New("pack returned an invalid entity")
		}
		key := fmt.Sprintf("%s:%s#%s:%s", request.Language, item.Path, item.Kind, item.QualifiedName)
		if _, exists := keys[key]; exists {
			return nil, fmt.Errorf("pack returned duplicate entity %q", key)
		}
		keys[key] = struct{}{}
		entities = append(entities, domain.Entity{Language: string(request.Language), Key: key, Kind: item.Kind, Name: item.Name, Signature: item.Signature, Path: item.Path, StartLine: item.StartLine, EndLine: item.EndLine, SourceSHA: request.SourceSHA, StructuralHash: item.StructuralHash})
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].Key < entities[j].Key })
	return entities, nil
}
