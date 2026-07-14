package planner

import (
	"context"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

const WorkerVersion = "1"

const maxEvidenceEntities = 100000

type SourceMode string

const (
	SourcePreview SourceMode = "preview"
	SourceFinal   SourceMode = "final"
)

type SourceRequest struct {
	Mode          SourceMode `json:"mode"`
	RepositoryID  string     `json:"repository_id"`
	TargetRef     string     `json:"target_ref"`
	RepositoryURL string     `json:"repository_url"`
	Credential    string     `json:"credential,omitempty"`
	BaseSHA       string     `json:"base_sha,omitempty"`
	HeadSHA       string     `json:"head_sha,omitempty"`
	FinalSHA      string     `json:"final_sha,omitempty"`
}

type SourceEvidence struct {
	RepositoryID      string                             `json:"repository_id"`
	TargetRef         string                             `json:"target_ref"`
	Mode              SourceMode                         `json:"mode"`
	SourceSHA         string                             `json:"source_sha"`
	PreviewIdentity   string                             `json:"preview_identity,omitempty"`
	BaseSHA           string                             `json:"base_sha,omitempty"`
	HeadSHA           string                             `json:"head_sha,omitempty"`
	GitTreeDigest     string                             `json:"git_tree_digest"`
	EntityShapeDigest string                             `json:"entity_shape_digest"`
	Entities          []domain.Entity                    `json:"entities"`
	Provenance        []domain.ContextSnapshotProvenance `json:"provenance"`
	CoverageComplete  bool                               `json:"coverage_complete"`
	WorkerVersion     string                             `json:"worker_version"`
}

type SourceRunner interface {
	IndexSource(ctx context.Context, request SourceRequest) (SourceEvidence, error)
}

type RunnerClaim struct {
	JobID        string
	LeaseOwner   string
	FencingToken int64
}

type ClaimedSourceRunner interface {
	IndexClaimedSource(ctx context.Context, claim RunnerClaim, request SourceRequest) (SourceEvidence, error)
}

type SourceIndexer interface {
	Index(ctx context.Context, root, sourceSHA string) ([]domain.LanguageProjection, error)
}

type NativeConfig struct {
	Indexer SourceIndexer
	TempDir string
}

type NativeRunner struct {
	indexer SourceIndexer
	tempDir string
}

type ProcessRunner struct {
	Path           string
	Timeout        time.Duration
	MaxOutputBytes int
}
