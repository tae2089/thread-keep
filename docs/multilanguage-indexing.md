# Multi-language Indexing Design

## Decision

Thread Keep borrows the structural-extraction approach of tools such as sem: detect languages automatically, extract entities per language, normalize them into one model, and index the snapshot safely.

It does not depend on, invoke, or embed sem. Go remains a built-in go/ast indexer. Non-Go support is delivered as Thread Keep-owned, separately installed language packs that may use Tree-sitter internally. No pack is downloaded, upgraded, or installed implicitly.

## Planning-grill decisions

| Dimension | Decision |
| --- | --- |
| Goal | Index mixed-language repositories without users selecting parsers and without shipping every grammar in the core binary. |
| Scope | Local entity extraction, coverage, update/status/commit rules, and trusted pack lifecycle. |
| Non-goal | sem integration, remote packs, arbitrary third-party plugins, graph analysis, source mutation, and automatic installs. |
| Core constraint | The core binary contains its built-in Go indexer and a small extension/name detector only. |
| Completeness | A snapshot is complete only when every detected, non-ignored language is fresh for the current source SHA. |
| Failure rule | Never claim complete coverage when an adapter is missing or fails; preserve the prior projection. |
| Commit rule | Partial coverage is queryable but cannot advance the canonical context ref. |

An ADR is intentionally skipped. The project uses focused architecture documents rather than an ADR collection.

## Canonical terms

| Term | Definition |
| --- | --- |
| Language detector | Core-owned file classifier that maps repository-relative paths to known language IDs. It does not parse source. |
| Language candidate | A detected language and its sorted file allowlist for one source SHA. |
| Indexer | A module with one small interface that produces normalized entities for one language candidate. |
| Built-in indexer | An indexer linked into the core binary. Go is the first and initially only one. |
| Language pack | A Thread Keep-owned executable installed separately from the core. It implements the indexer protocol and may use Tree-sitter internally. |
| Indexer catalog | Core-owned registry that resolves a candidate to a built-in indexer, installed pack, or no implementation. |
| Coverage | Observable freshness and availability state of one detected language in one worktree. |
| Projection | SQLite current entity/search state. It is derived data, not immutable history. |
| Canonical entity ID | Thread Keep-owned identity used by notes and context objects. Parser-native IDs are evidence, never canonical storage. |
| Selector | Human-readable entity reference resolved to one canonical entity ID. |

Use language pack rather than plugin in the first release. The pack set is owned and versioned by Thread Keep; there is no general third-party marketplace or in-process code loading.

## Component design

~~~mermaid
flowchart LR
    subgraph core["Thread Keep core (Go)"]
        CLI["Cobra update command"]
        App["Index coordinator"]
        Detector["Language detector"]
        Catalog["Indexer catalog"]
        GoIndexer["Built-in Go indexer"]
        Normalizer["Entity normalizer"]
        Store["SQLite projection and coverage store"]
    end
    subgraph packs["Optional Thread Keep language packs"]
        TSPack["TypeScript pack (Tree-sitter)"]
        JSPack["JavaScript pack (Tree-sitter)"]
        PyPack["Python pack (Tree-sitter)"]
        JavaPack["Java pack (Tree-sitter)"]
        KotlinPack["Kotlin pack (Tree-sitter)"]
        RustPack["Rust pack (Tree-sitter)"]
    end
    CLI --> App
    App --> Detector
    App --> Catalog
    Catalog --> GoIndexer
    Catalog --> TSPack
    Catalog --> JSPack
    Catalog --> PyPack
    Catalog --> JavaPack
    Catalog --> KotlinPack
    Catalog --> RustPack
    GoIndexer --> Normalizer
    TSPack --> Normalizer
    JSPack --> Normalizer
    PyPack --> Normalizer
    JavaPack --> Normalizer
    KotlinPack --> Normalizer
    RustPack --> Normalizer
    Normalizer --> Store
~~~

Core owns the interface and launches packs over local stdio. Packs never import core packages and do not access SQLite directly.

Current integration points are Service.Update in internal/app/service.go:57-76, the concrete Go extractor in internal/indexer/go.go:20-95, the entity model in internal/domain/domain.go:58-68, and atomic entity replacement in internal/store/store.go:67-82.

## Module boundary

Create internal/indexing. Its small interface is:

~~~go
type Indexer interface {
    Descriptor() Descriptor
    Index(context.Context, Request) (Result, error)
}

type Request struct {
    RepositoryRoot string
    SourceSHA      string
    Language       LanguageID
    Files          []string // sorted, repository-relative
}

type Result struct {
    Language       LanguageID
    IndexerID      string
    IndexerVersion string
    Entities       []ExtractedEntity
    Diagnostics    []Diagnostic
}
~~~

IndexCoordinator hides detection, selection, subprocess lifecycle, protocol validation, duplicate detection, coverage transitions, and atomic projection replacement. Service receives one UpdateResult; it does not know which parser ran.

The seam is real because two adapter modes exist:

1. A built-in Go adapter wraps the current go/ast extractor.
2. An installed language-pack adapter launches a trusted local executable.

Do not add interfaces around Store or Git for this work. Existing concrete local implementations remain the test surface.

## Detection and pack lifecycle

The detector uses a versioned, core-owned extension/name table:

| Language ID | Candidate evidence | Initial resolver |
| --- | --- | --- |
| go | .go | built-in Go adapter |
| typescript | .ts, .tsx, .mts, .cts | TypeScript pack when installed |
| javascript | .js, .jsx, .mjs, .cjs | JavaScript pack when installed |
| python | .py, .pyi, .pyw | Python pack when installed |
| java | .java | Java pack when installed |
| kotlin | .kt, .kts | Kotlin pack when installed |
| rust | .rs | Rust pack when installed |

It skips .git, vendor, and node_modules, matching the current Go indexer. It sorts paths and does not parse source.

~~~text
thread-keep init
  -> report detected languages and installed/missing coverage

thread-keep indexers list
  -> read-only report of known builtin/installed/missing indexers and detected languages

thread-keep update
  -> automatically select installed packs for detected languages

thread-keep indexers install --detected
  -> explicit installation boundary for detected official packs
~~~

Packs live in the user configuration directory, not each repository. The core resolves fixed official TypeScript, JavaScript, Python, Java, Kotlin, and Rust pack filenames there and records their ID/version from the protocol. A release binary embeds an Ed25519 public key and accepts the official GitHub Releases signed manifest only. `indexers install --detected` verifies the signed manifest, selects a current-platform asset, verifies byte size and SHA-256, then atomically publishes it. Repositories store coverage/provenance only and never a downloaded executable.

## Pack protocol

Packs are local processes with a versioned JSON Lines protocol over stdin/stdout:

~~~text
thread-keep-index-<language> index --protocol-version=1
~~~

Request:

~~~json
{
  "protocol_version": 1,
  "repository_root": "/absolute/repo",
  "source_sha": "abc123",
  "language": "typescript",
  "files": ["apps/web/src/auth.ts"]
}
~~~

Result:

~~~json
{
  "protocol_version": 1,
  "indexer": {"id": "typescript", "version": "1.0.0"},
  "language": "typescript",
  "entities": [
    {
      "path": "apps/web/src/auth.ts",
      "kind": "function",
      "name": "validateToken",
      "qualified_name": "validateToken",
      "signature": "function validateToken(token: string): boolean",
      "start_line": 12,
      "end_line": 18,
      "structural_hash": "..."
    }
  ],
  "diagnostics": []
}
~~~

The core rejects a response with an invalid protocol version, language, path allowlist, line range, identity, or hash. It enforces a timeout and bounded stdout size. Stderr is kept as a diagnostic and never mixed into protocol JSON.

A pack may use Tree-sitter, a compiler API, or another parser. That choice does not cross the protocol boundary.

## Entity identity and migration

The first compatible implementation retains existing Go entity keys so pending notes and immutable v1 objects remain directly addressable. It adds `language = go` to existing rows without changing their key. External packs use a language- and path-qualified key:

~~~text
<language>:<repository-relative-path>#<kind>:<qualified-name>
~~~

This prevents cross-language collisions while keeping `note add` and `context get` familiar. An opaque-ID/alias migration remains deferred; it must not be claimed as implemented until it rewrites pending bindings and provides v1 aliases atomically.

## Coverage and freshness

Coverage is stored per worktree and language:

~~~text
language
detected_file_count
state                 indexed | missing_pack | failed
source_sha
indexer_id
indexer_version
diagnostic
updated_at
~~~

Coverage is complete only if every detected, non-ignored language is indexed at the current Git HEAD.

- A Go + external-language repository without the detected pack may still update fresh Go entities, but reports incomplete coverage.
- If an external language was indexed previously and its pack becomes unavailable after a source change, old rows remain recovery data but are excluded from current search/context results.
- Commit rejects incomplete coverage with typed coverage_incomplete. It never creates history from a mixed-freshness snapshot.
- Note add accepts only fresh entities.

Status and JSON output expose coverage_complete plus a coverage array. Default update returns a successful partial result with explicit coverage. Future update --require-complete returns coverage_incomplete after persisting coverage observation.

## Proposed update flow

~~~text
P1  RECEIVE update(requireComplete)                         [internal/app/service.go:57]
P2  READ mutable Git state and require a clean worktree     [internal/app/service.go:58-68]
P3  DETECT language candidates from the source tree
P4  READ existing coverage and pending-note state
P5  IF source changed while pending notes exist
P6    RETURN working_set_dirty; write no projection or coverage mutation
P7  RESOLVE each candidate through the indexer catalog
P8  FOR EACH installed/built-in indexer
P9    CALL indexer with the candidate allowlist
P10     IF protocol or extraction fails -> record failed coverage candidate
P11 IF any selected indexer failed
P12   WRITE failed coverage and preserve that language's entity projection
P13   RETURN typed index failure
P14 NORMALIZE every successful result into language-qualified entity keys
P15 IF identities duplicate -> record failed coverage; preserve projection; return validation
P16 WRITE successful language projections and coverage rows in one SQLite transaction
P17 REBUILD search only from fresh coverage
P18 WRITE missing coverage rows; remove coverage for languages no longer detected
P19 IF requireComplete AND coverage incomplete -> return coverage_incomplete
P20 RETURN update result with source SHA, entity count, and coverage
~~~

P12 and P16 are intentionally separate transactions. A selected adapter failure is observable but cannot partially replace a projection. A missing pack is not selected, so a successful Go result may advance while status remains incomplete.

~~~mermaid
flowchart TD
    A["Receive update"] --> B["Detect language candidates"]
    B --> C{"Pending notes and source changed?"}
    C -- yes --> D["Return working_set_dirty"]
    C -- no --> E["Resolve built-in or installed pack"]
    E --> F{"Any selected indexer fails?"}
    F -- yes --> G["Persist failure coverage only"]
    G --> H["Return typed index failure"]
    F -- no --> I["Normalize and validate identities"]
    I --> J{"Duplicate or invalid identity?"}
    J -- yes --> G
    J -- no --> K["Atomically replace successful language projections"]
    K --> L["Rebuild fresh-only search"]
    L --> M{"Require complete and coverage incomplete?"}
    M -- yes --> N["Return coverage_incomplete"]
    M -- no --> O["Return update result with coverage"]
~~~

Completeness: P2 covers empty candidates; P5-P6 preserve the pending-note guard; P9-P13 cover process/protocol failure before projection mutation; P14-P17 cover validation and atomic writes; P18-P19 make partial coverage visible. Indexers run sequentially first; parallelism is deferred until cancellation and memory limits need it.

## CLI contract

Additional future commands:

~~~text
<additional language pack commands as new official packs are introduced>
~~~

`thread-keep update --require-complete`, `thread-keep indexers list`, and `thread-keep indexers install --detected` are implemented. The installer is explicit, never runs during init/update, and requires a release-binary public key.

### Signed official manifest

The release manifest is an envelope rather than a JSON canonicalization scheme:

~~~json
{
  "payload": "base64-encoded raw manifest JSON bytes",
  "signature": "base64-encoded Ed25519 signature over payload bytes"
}
~~~

The decoded payload has `schema_version: 1` and one or more official pack entries. Each entry identifies a pack ID, pack version, protocol version, and assets. Each asset contains `goos`, `goarch`, the official HTTPS release URL, exact byte `size`, and lowercase `sha256`. The release binary gets its base64 public key through `make release-build THREAD_KEEP_MANIFEST_PUBLIC_KEY_B64=...`; the private key is secure release infrastructure input, never repository content. `cmd/thread-keep-sign-manifest` signs a supplied raw payload and emits only the envelope.

Status JSON gains:

~~~json
{
  "coverage_complete": false,
  "coverage": [
    {"language": "go", "state": "indexed", "source_sha": "abc123"},
    {"language": "typescript", "state": "missing_pack", "source_sha": "abc123"}
  ]
}
~~~

UpdateResult gains the same coverage summary. Scripts branch on coverage_incomplete, not an undocumented numeric exit code.

## Test scenarios

- [x] Go only: built-in Go produces indexed complete coverage.
- [x] Mixed repository with the TypeScript pack: Go and TypeScript projections can commit.
- [x] Missing pack: Go is fresh, the detected external language is missing_pack, search excludes it, and commit rejects.
- [x] JavaScript, Python, Java, Kotlin, and Rust pack units, fixed-path catalog, signed-manifest selection, and missing-coverage behavior are verified locally. The added mixed-language Docker E2E scenarios are deferred until the existing execution quota permits them.
- [x] Strict update: incomplete coverage persists and update --require-complete returns coverage_incomplete.
- [x] Adapter failure and protocol rejection preserve the failed language projection and record failed coverage.
- [x] Cross-language identity: equivalent names in Go, TypeScript, JavaScript, Python, Java, Kotlin, and Rust have distinct keys.
- [x] Migration safety: v1 Go rows gain language coverage without rewriting note bindings or objects.
- [x] No implicit install: init/update makes no network request or adapter-executable write.

## Implementation sequence

1. Add domain terms, coverage schema, and v1-to-v2 migration tests.
2. Extract the Go indexer behind internal/indexing.Indexer and preserve behavior through contract tests.
3. Add detector/catalog, coverage coordinator, and fresh-only search filtering.
4. Add subprocess pack runner with a fake local pack for protocol, timeout, and failure tests.
5. Add CLI coverage fields, require-complete, commit gate, and error contracts.
6. Implement official TypeScript, JavaScript, Python, Java, Kotlin, and Rust Tree-sitter packs and mixed-repository Docker E2E fixtures. [implemented; Docker execution remains quota-deferred]
7. Add explicit official-pack installation, signed-manifest authentication, checksum verification, and target-platform handling. [implemented]

No Tree-sitter library enters the Go core. It appears only in separate external-pack dependency graphs.

## Risks and constraints

- Parser drift: record pack ID/version and require reindex after extraction-schema upgrades.
- Untrusted execution: repository-defined executables are forbidden. The installer accepts only a release-binary-verified signed manifest and official GitHub Release/CDN redirects, verifies exact artifact size and SHA-256, and atomically publishes without replacing an executable target. Release-key and asset provisioning remain secure release-infrastructure work.
- Mixed freshness: partial projections aid exploration but are never committable history.
- Migration correctness: immutable v1 objects are never rewritten; aliases preserve history.
- Performance: explicit update may start local processes; no daemon or watcher is introduced.
- Unsupported files: report coverage. Do not invent arbitrary text chunks as code entities.

## Handoff

Planning-grill status: SHARPENED. Terminology, interface, state transitions, failure policy, implementation sequence, and test scenarios are concrete enough to decompose when implementation is requested.
