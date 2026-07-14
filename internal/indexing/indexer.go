package indexing

import (
	"context"
	"fmt"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/indexer"
)

type Descriptor struct {
	ID      string
	Version string
}

type Request struct {
	RepositoryRoot string
	SourceSHA      string
	Language       Language
	Files          []string
}

type Result struct {
	Indexer     Descriptor
	Entities    []domain.Entity
	Diagnostics []string
}

type Indexer interface {
	Descriptor() Descriptor
	Index(context.Context, Request) (Result, error)
}

type GoIndexer struct {
	delegate indexer.Go
}

type Coordinator struct {
	Go    Indexer
	Packs map[Language]Indexer
}

func (GoIndexer) Descriptor() Descriptor { return Descriptor{ID: "builtin/go", Version: "1"} }

func (g GoIndexer) Index(ctx context.Context, request Request) (Result, error) {
	entities, err := g.delegate.IndexFiles(ctx, request.RepositoryRoot, request.SourceSHA, request.Files)
	if err != nil {
		return Result{}, err
	}
	for index := range entities {
		entities[index].Language = string(Go)
	}
	return Result{Indexer: g.Descriptor(), Entities: entities}, nil
}

func NewCoordinator() Coordinator {
	coordinator := Coordinator{Go: GoIndexer{}, Packs: map[Language]Indexer{}}
	for _, language := range externalPackLanguages {
		if pack, found := FindInstalledPack(language); found {
			coordinator.Packs[language] = pack
		}
	}
	return coordinator
}

func (c Coordinator) Index(ctx context.Context, root, sourceSHA string) ([]domain.LanguageProjection, error) {
	candidates, err := DetectContext(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("detect supported languages: %w", err)
	}
	projections := make([]domain.LanguageProjection, 0, len(candidates))
	for _, candidate := range candidates {
		indexer, found := c.forLanguage(candidate.Language)
		if !found {
			projections = append(projections, domain.LanguageProjection{Coverage: domain.Coverage{
				Language: string(candidate.Language), State: domain.CoverageMissingPack, IndexerID: packID(candidate.Language), SourceSHA: sourceSHA,
			}})
			continue
		}
		result, err := indexer.Index(ctx, Request{RepositoryRoot: root, SourceSHA: sourceSHA, Language: candidate.Language, Files: candidate.Files})
		if err != nil {
			projections = append(projections, domain.LanguageProjection{Coverage: domain.Coverage{
				Language: string(candidate.Language), State: domain.CoverageFailed, IndexerID: indexer.Descriptor().ID, IndexerVersion: indexer.Descriptor().Version, SourceSHA: sourceSHA, Detail: err.Error(),
			}})
			continue
		}
		for index := range result.Entities {
			result.Entities[index].Language = string(candidate.Language)
		}
		descriptor := result.Indexer
		if descriptor.ID == "" {
			descriptor = indexer.Descriptor()
		}
		if err := validateResult(candidate, sourceSHA, indexer.Descriptor(), descriptor, result.Entities); err != nil {
			projections = append(projections, domain.LanguageProjection{Coverage: domain.Coverage{
				Language: string(candidate.Language), State: domain.CoverageFailed, IndexerID: indexer.Descriptor().ID, IndexerVersion: indexer.Descriptor().Version, SourceSHA: sourceSHA, Detail: err.Error(),
			}})
			continue
		}
		projections = append(projections, domain.LanguageProjection{Coverage: domain.Coverage{
			Language: string(candidate.Language), State: domain.CoverageIndexed, IndexerID: descriptor.ID, IndexerVersion: descriptor.Version, SourceSHA: sourceSHA,
		}, Entities: result.Entities})
	}
	return projections, nil
}

func validateResult(candidate Candidate, sourceSHA string, expected, actual Descriptor, entities []domain.Entity) error {
	if actual.ID != expected.ID || actual.Version == "" || expected.Version != "" && actual.Version != expected.Version {
		return fmt.Errorf("indexer descriptor mismatch: got %q version %q", actual.ID, actual.Version)
	}
	allowed := make(map[string]struct{}, len(candidate.Files))
	for _, file := range candidate.Files {
		allowed[file] = struct{}{}
	}
	entitiesByKey := make(map[string]domain.Entity, len(entities))
	for _, entity := range entities {
		if _, found := allowed[entity.Path]; !found || !validRelativePath(entity.Path) {
			return fmt.Errorf("indexer returned an unrequested path %q", entity.Path)
		}
		if entity.Key == "" || entity.Name == "" || entity.Language != string(candidate.Language) || entity.SourceSHA != sourceSHA || entity.StartLine <= 0 || entity.EndLine < entity.StartLine || entity.StructuralHash == "" || !validKind(entity.Kind) {
			return fmt.Errorf("indexer returned an invalid entity %q", entity.Key)
		}
		if _, found := entitiesByKey[entity.Key]; found {
			return fmt.Errorf("indexer returned duplicate entity key %q", entity.Key)
		}
		entitiesByKey[entity.Key] = entity
	}
	return nil
}

func (c Coordinator) forLanguage(language Language) (Indexer, bool) {
	if language == Go && c.Go != nil {
		return c.Go, true
	}
	indexer, found := c.Packs[language]
	return indexer, found
}

func packID(language Language) string {
	return "thread-keep-index-" + string(language)
}
