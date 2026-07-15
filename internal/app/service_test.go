package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/gitrepo"
	"github.com/tae2089/thread-keep/internal/remote"
	"github.com/tae2089/thread-keep/internal/remote/server"
	"github.com/tae2089/thread-keep/internal/store"
)

type indexCoordinatorFunc func(context.Context, string, string) ([]domain.LanguageProjection, error)

func TestInitAndUpdateIndexGoEntities(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"example.go": `package example

type Worker struct{}

func Run(input string) error { return nil }

func (w *Worker) Process() {}
`,
	})

	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()

	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := os.Stat(localDatabasePath(repo)); err != nil {
		t.Fatalf("index database missing: %v", err)
	}
	command := exec.Command("git", "-C", repo, "status", "--porcelain")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, output)
	}
	if len(output) != 0 {
		t.Fatalf("git status after Init() = %q, want clean", output)
	}

	result, err := svc.Update(context.Background())
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if result.IndexedEntities != 3 {
		t.Fatalf("IndexedEntities = %d, want 3", result.IndexedEntities)
	}

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.EntityCount != 3 {
		t.Fatalf("EntityCount = %d, want 3", status.EntityCount)
	}
}

func TestStatusMigratesLegacyStorageIntoWorktree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	state, err := gitrepo.Discover(ctx, repo)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	legacyLayout := store.LegacyLayout(state.CommonDir)
	legacyStore, err := store.Open(ctx, legacyLayout)
	if err != nil {
		t.Fatalf("Open(legacy) error = %v", err)
	}
	key := domain.WorkingSetKey{RepositoryID: state.RepositoryID, WorktreeID: state.WorktreeID, RefName: state.RefName(), SourceSHA: state.HeadSHA}
	entity := domain.Entity{Language: "go", Key: "example.Run", Kind: "function", Name: "Run", Signature: "func()", Path: "example.go", StartLine: 3, EndLine: 3, SourceSHA: state.HeadSHA, StructuralHash: "hash"}
	projection := domain.LanguageProjection{Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: state.HeadSHA}, Entities: []domain.Entity{entity}}
	if err := legacyStore.ApplyIndexUpdate(ctx, key, []domain.LanguageProjection{projection}); err != nil {
		t.Fatalf("ApplyIndexUpdate(legacy) error = %v", err)
	}
	if err := legacyStore.Close(); err != nil {
		t.Fatalf("Close(legacy) error = %v", err)
	}

	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	status, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.EntityCount != 1 || status.SourceSHA != state.HeadSHA {
		t.Fatalf("Status() = %+v, want migrated entity at current source", status)
	}
	if _, err := os.Stat(filepath.Join(repo, ".thread-keep", "index.sqlite")); err != nil {
		t.Fatalf("current database missing: %v", err)
	}
	if _, err := os.Stat(legacyLayout.Database); err != nil {
		t.Fatalf("legacy database was not preserved: %v", err)
	}
}

func TestUpdateRecordsMissingTypeScriptCoverageAndCommitRejectsIt(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"example.go": "package example\nfunc Run() {}\n",
		"web/app.ts": "export function run(): void {}\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	result, err := svc.Update(context.Background())
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if result.IndexedEntities != 1 || result.CoverageComplete {
		t.Fatalf("Update() = %+v, want one Go entity and incomplete coverage", result)
	}
	if len(result.Coverage) != 2 || result.Coverage[1].Language != "typescript" || result.Coverage[1].State != domain.CoverageMissingPack {
		t.Fatalf("Update() coverage = %#v, want missing TypeScript pack", result.Coverage)
	}
	if _, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "keep run simple", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(context.Background(), CommitInput{Message: "must wait", Author: "tester"}); domain.CodeOf(err) != domain.CodeCoverageIncomplete {
		t.Fatalf("Commit() error = %v, want coverage incomplete", err)
	}
}

func TestUpdateRecordsMissingJavaScriptCoverageAndCommitRejectsIt(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"example.go": "package example\nfunc Run() {}\n",
		"web/app.js": "export function run() {}\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	result, err := svc.Update(context.Background())
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if result.IndexedEntities != 1 || result.CoverageComplete || len(result.Coverage) != 2 || result.Coverage[1].Language != "javascript" || result.Coverage[1].State != domain.CoverageMissingPack {
		t.Fatalf("Update() = %+v, want one Go entity and missing JavaScript coverage", result)
	}
	if _, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "wait for JavaScript coverage", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(context.Background(), CommitInput{Message: "must wait", Author: "tester"}); domain.CodeOf(err) != domain.CodeCoverageIncomplete {
		t.Fatalf("Commit() error = %v, want coverage incomplete", err)
	}
}

func TestUpdateRecordsMissingPythonCoverageAndCommitRejectsIt(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"example.go":      "package example\nfunc Run() {}\n",
		"services/app.py": "def run():\n    return None\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	result, err := svc.Update(context.Background())
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if result.IndexedEntities != 1 || result.CoverageComplete || len(result.Coverage) != 2 || result.Coverage[1].Language != "python" || result.Coverage[1].State != domain.CoverageMissingPack {
		t.Fatalf("Update() = %+v, want one Go entity and missing Python coverage", result)
	}
	if _, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "wait for Python coverage", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(context.Background(), CommitInput{Message: "must wait", Author: "tester"}); domain.CodeOf(err) != domain.CodeCoverageIncomplete {
		t.Fatalf("Commit() error = %v, want coverage incomplete", err)
	}
}

func TestUpdateRecordsMissingJavaCoverageAndCommitRejectsIt(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"example.go":    "package example\nfunc Run() {}\n",
		"src/Main.java": "class Main { void run() {} }\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	result, err := svc.Update(context.Background())
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if result.IndexedEntities != 1 || result.CoverageComplete || len(result.Coverage) != 2 || result.Coverage[1].Language != "java" || result.Coverage[1].State != domain.CoverageMissingPack {
		t.Fatalf("Update() = %+v, want one Go entity and missing Java coverage", result)
	}
	if _, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "wait for Java coverage", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(context.Background(), CommitInput{Message: "must wait", Author: "tester"}); domain.CodeOf(err) != domain.CodeCoverageIncomplete {
		t.Fatalf("Commit() error = %v, want coverage incomplete", err)
	}
}

func TestUpdateRecordsMissingKotlinCoverageAndCommitRejectsIt(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"example.go": "package example\nfunc Run() {}\n",
		"src/App.kt": "class App { fun run() {} }\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	result, err := svc.Update(context.Background())
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if result.IndexedEntities != 1 || result.CoverageComplete || len(result.Coverage) != 2 || result.Coverage[1].Language != "kotlin" || result.Coverage[1].State != domain.CoverageMissingPack {
		t.Fatalf("Update() = %+v, want one Go entity and missing Kotlin coverage", result)
	}
	if _, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "wait for Kotlin coverage", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(context.Background(), CommitInput{Message: "must wait", Author: "tester"}); domain.CodeOf(err) != domain.CodeCoverageIncomplete {
		t.Fatalf("Commit() error = %v, want coverage incomplete", err)
	}
}

func TestUpdateRecordsMissingRustCoverageAndCommitRejectsIt(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"example.go":             "package example\nfunc Run() {}\n",
		"crates/core/src/lib.rs": "pub struct Service;\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	result, err := svc.Update(context.Background())
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if result.IndexedEntities != 1 || result.CoverageComplete || len(result.Coverage) != 2 || result.Coverage[1].Language != "rust" || result.Coverage[1].State != domain.CoverageMissingPack {
		t.Fatalf("Update() = %+v, want one Go entity and missing Rust coverage", result)
	}
	if _, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "wait for Rust coverage", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(context.Background(), CommitInput{Message: "must wait", Author: "tester"}); domain.CodeOf(err) != domain.CodeCoverageIncomplete {
		t.Fatalf("Commit() error = %v, want coverage incomplete", err)
	}
}

func TestUpdateDropsCoverageForLanguagesNoLongerDetected(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"example.go": "package example\nfunc Run() {}\n",
		"web/app.ts": "export function run(): void {}\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); err != nil {
		t.Fatalf("initial Update() error = %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "web", "app.ts")); err != nil {
		t.Fatalf("remove TypeScript source: %v", err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "remove TypeScript source")
	result, err := svc.Update(context.Background())
	if err != nil {
		t.Fatalf("Update() after removal error = %v", err)
	}
	if !result.CoverageComplete || len(result.Coverage) != 1 || result.Coverage[0].Language != "go" {
		t.Fatalf("Update() after removal = %+v, want only complete Go coverage", result)
	}
}

func TestUpdateRejectsWorktreeChangeBeforePersistingProjections(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	delegate := svc.indexer
	svc.indexer = indexCoordinatorFunc(func(ctx context.Context, root, sourceSHA string) ([]domain.LanguageProjection, error) {
		projections, err := delegate.Index(ctx, root, sourceSHA)
		if err != nil {
			return nil, err
		}
		writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() { println(\"changed\") }\n")
		return projections, nil
	})

	if _, err := svc.Update(context.Background()); domain.CodeOf(err) != domain.CodeRepositoryState {
		t.Fatalf("Update() error = %v, want repository state", err)
	}
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.EntityCount != 0 {
		t.Fatalf("EntityCount = %d, want no persisted projection", status.EntityCount)
	}
}

func TestOpenRejectsNonGitDirectory(t *testing.T) {
	t.Parallel()
	_, err := Open(context.Background(), t.TempDir())
	if domain.CodeOf(err) != domain.CodeRepositoryState {
		t.Fatalf("Open() error = %v, want repository state", err)
	}
}

func TestPendingNoteSearchAndCommit(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"payment.go": `package payment

func Authorize() error { return nil }
`,
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	note, err := svc.AddNote(context.Background(), AddNoteInput{
		EntityKey: "payment.Authorize",
		Kind:      "constraint",
		Body:      "결제 승인 재시도는 중복 청구를 만들면 안 된다.",
		Author:    "tester",
	})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if !note.Pending {
		t.Fatal("note must be pending before commit")
	}

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.PendingNotes != 1 || status.ContextCommitID != "" {
		t.Fatalf("unexpected pre-commit status: %+v", status)
	}

	hits, err := svc.Search(context.Background(), "중복 청구")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	exactHits, err := svc.Search(context.Background(), "payment.Authorize")
	if err != nil {
		t.Fatalf("Search() exact-key error = %v", err)
	}
	symbolHits, err := svc.Search(context.Background(), "Authorize")
	if err != nil {
		t.Fatalf("Search() symbol error = %v", err)
	}
	if len(symbolHits) != 1 || symbolHits[0].EntityKey != "payment.Authorize" {
		t.Fatalf("unexpected symbol hits: %+v", symbolHits)
	}
	if len(exactHits) != 1 || exactHits[0].EntityKey != "payment.Authorize" || !containsMatchField(exactHits[0].MatchedFields, domain.SearchMatchEntityKey) || !exactHits[0].Fresh {
		t.Fatalf("unexpected exact-key evidence: %+v", exactHits)
	}
	if !containsMatchField(symbolHits[0].MatchedFields, domain.SearchMatchName) || !symbolHits[0].Fresh {
		t.Fatalf("unexpected symbol evidence: %+v", symbolHits)
	}
	if len(hits) != 1 || hits[0].EntityKey != "payment.Authorize" || !hits[0].Pending || !containsMatchField(hits[0].MatchedFields, domain.SearchMatchNoteBody) || len(hits[0].NoteIDs) != 1 || hits[0].NoteIDs[0] != note.ID || hits[0].BindingState != domain.NoteBindingActive || !hits[0].Fresh {
		t.Fatalf("unexpected search hits: %+v", hits)
	}

	commit, err := svc.Commit(context.Background(), CommitInput{Message: "document payment retry", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if commit.ID == "" {
		t.Fatal("commit ID is empty")
	}
	contents, err := os.ReadFile(localObjectPath(repo, commit.ID))
	if err != nil {
		t.Fatalf("read context object: %v", err)
	}
	var object domain.ContextObject
	if err := json.Unmarshal(contents, &object); err != nil {
		t.Fatalf("decode context object: %v", err)
	}
	if object.RefName != commit.RefName || object.SourceSHA != commit.SourceSHA || object.Message != commit.Message || object.Author != commit.Author {
		t.Fatalf("context object metadata = %+v, commit = %+v", object, commit)
	}
	if object.SchemaVersion != 3 || len(object.ParentIDs) != 0 || len(object.Provenance) != 1 || object.Provenance[0].Language != "go" || object.Provenance[0].IndexerID != "builtin/go" || object.Provenance[0].IndexerVersion != "1" || object.Provenance[0].SourceSHA != commit.SourceSHA || len(object.Entities) != 1 || len(object.Notes) != 1 || object.Notes[0].Body != note.Body || object.Notes[0].Pending {
		t.Fatalf("context object contents = %+v", object)
	}
	if object.Notes[0].RevisionID == "" || object.Notes[0].RevisionID == object.Notes[0].ID || object.Notes[0].BindingState != domain.NoteBindingActive || object.Notes[0].BindingSourceSHA != commit.SourceSHA {
		t.Fatalf("context object note revision/binding = %+v", object.Notes[0])
	}
	if len(object.RevisionMappings) != 1 || object.RevisionMappings[0].EntityKey != object.Notes[0].EntityKey || object.RevisionMappings[0].NoteID != object.Notes[0].ID || object.RevisionMappings[0].RevisionID != object.Notes[0].RevisionID || object.RevisionMappings[0].BindingState != object.Notes[0].BindingState || object.RevisionMappings[0].BindingSourceSHA != object.Notes[0].BindingSourceSHA || object.RevisionMappings[0].ReviewReason != object.Notes[0].ReviewReason {
		t.Fatalf("context object revision mappings = %+v", object.RevisionMappings)
	}

	status, err = svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() after commit error = %v", err)
	}
	if status.PendingNotes != 0 || status.ContextCommitID != commit.ID {
		t.Fatalf("unexpected post-commit status: %+v", status)
	}
}

func containsMatchField(fields []domain.SearchMatchField, want domain.SearchMatchField) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}

func TestReviseNoteCreatesNewImmutableRevision(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	first, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "first revision", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	firstCommit, err := svc.Commit(ctx, CommitInput{Message: "first note", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(first) error = %v", err)
	}

	revised, err := svc.ReviseNote(ctx, ReviseNoteInput{NoteID: first.ID, Body: "second revision", Author: "reviewer"})
	if err != nil {
		t.Fatalf("ReviseNote() error = %v", err)
	}
	if !revised.Pending || revised.ID != first.ID || revised.RevisionID == first.RevisionID || revised.SupersedesRevisionID != first.RevisionID || revised.Body != "second revision" || revised.BindingState != domain.NoteBindingActive {
		t.Fatalf("revised note = %+v", revised)
	}
	second, err := svc.Commit(ctx, CommitInput{Message: "revised note", Author: "reviewer"})
	if err != nil {
		t.Fatalf("Commit(second) error = %v", err)
	}
	contents, err := os.ReadFile(localObjectPath(repo, second.ID))
	if err != nil {
		t.Fatalf("ReadFile(second object): %v", err)
	}
	var object domain.ContextObject
	if err := json.Unmarshal(contents, &object); err != nil {
		t.Fatalf("Unmarshal(second object): %v", err)
	}
	if object.SchemaVersion != 3 || len(object.ParentIDs) != 1 || object.ParentIDs[0] != firstCommit.ID || len(object.Notes) != 1 || object.Notes[0].RevisionID != revised.RevisionID || object.Notes[0].SupersedesRevisionID != first.RevisionID {
		t.Fatalf("second object notes = %+v", object.Notes)
	}
}

func TestUpdateMarksChangedBoundNoteForReviewBeforeCommit(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(first) error = %v", err)
	}
	note, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "warning", Body: "keep behavior", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(ctx, CommitInput{Message: "document run", Author: "tester"}); err != nil {
		t.Fatalf("Commit(first) error = %v", err)
	}
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() { println(\"changed\") }\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "change run")

	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(changed) error = %v", err)
	}
	pending, err := svc.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != note.ID || pending[0].Body != note.Body || pending[0].RevisionID != note.RevisionID || pending[0].BindingState != domain.NoteBindingNeedsReview || pending[0].BindingSourceSHA == note.BindingSourceSHA || pending[0].ReviewReason != "structural_change" {
		t.Fatalf("pending reconciliation = %+v", pending)
	}
	contextResult, err := svc.Context(ctx, "example.Run")
	if err != nil {
		t.Fatalf("Context() error = %v", err)
	}
	if len(contextResult.Notes) != 0 {
		t.Fatalf("Context() notes = %+v, want needs-review note hidden", contextResult.Notes)
	}
	hits, err := svc.Search(ctx, "keep behavior")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("Search() hits = %+v, want needs-review note hidden", hits)
	}
	committed, err := svc.Commit(ctx, CommitInput{Message: "review changed binding", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(changed) error = %v", err)
	}
	contents, err := os.ReadFile(localObjectPath(repo, committed.ID))
	if err != nil {
		t.Fatalf("ReadFile(reconciled object): %v", err)
	}
	var object domain.ContextObject
	if err := json.Unmarshal(contents, &object); err != nil {
		t.Fatalf("Unmarshal(reconciled object): %v", err)
	}
	if len(object.Notes) != 1 || object.Notes[0].BindingState != domain.NoteBindingNeedsReview || object.Notes[0].BindingSourceSHA != committed.SourceSHA {
		t.Fatalf("reconciled object notes = %+v", object.Notes)
	}
}

func TestReviewNoteConfirmsNeedsReviewBindingWithoutChangingRevision(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(first) error = %v", err)
	}
	note, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "decision", Body: "preserve behavior", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(ctx, CommitInput{Message: "document run", Author: "tester"}); err != nil {
		t.Fatalf("Commit(first) error = %v", err)
	}
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() { println(\"changed\") }\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "change run")
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(changed) error = %v", err)
	}

	confirmed, err := svc.ReviewNote(ctx, ReviewNoteInput{NoteID: note.ID, EntityKey: "example.Run"})
	if err != nil {
		t.Fatalf("ReviewNote() error = %v", err)
	}
	if !confirmed.Pending || confirmed.RevisionID != note.RevisionID || confirmed.Body != note.Body || confirmed.BindingState != domain.NoteBindingActive || confirmed.BindingSourceSHA == note.BindingSourceSHA || confirmed.ReviewReason != "" {
		t.Fatalf("confirmed note = %+v", confirmed)
	}
	pending, err := svc.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if len(pending) != 1 || pending[0].BindingState != domain.NoteBindingActive {
		t.Fatalf("pending notes after review = %+v", pending)
	}
}

func TestReviewNoteRejectsActiveBindingWithoutWritingPendingState(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	note, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "decision", Body: "active", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(ctx, CommitInput{Message: "active note", Author: "tester"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, err := svc.ReviewNote(ctx, ReviewNoteInput{NoteID: note.ID, EntityKey: "example.Run"}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ReviewNote() error = %v, want validation", err)
	}
	pending, err := svc.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending notes after rejected review = %+v", pending)
	}
}

func TestRepeatedUpdatePreservesPendingRevisionAfterReconciliation(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(first) error = %v", err)
	}
	note, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "first", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(ctx, CommitInput{Message: "first note", Author: "tester"}); err != nil {
		t.Fatalf("Commit(first) error = %v", err)
	}
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() { println(\"changed\") }\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "change source")
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(changed) error = %v", err)
	}
	revised, err := svc.ReviseNote(ctx, ReviseNoteInput{NoteID: note.ID, Body: "human revision", Author: "reviewer"})
	if err != nil {
		t.Fatalf("ReviseNote() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(repeated) error = %v", err)
	}
	pending, err := svc.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != note.ID || pending[0].RevisionID != revised.RevisionID || pending[0].Body != "human revision" || pending[0].BindingState != domain.NoteBindingActive {
		t.Fatalf("pending note after repeated update = %+v", pending)
	}
}

func TestReviseNoteRejectsUncommittedRevision(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	note, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "uncommitted", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.ReviseNote(ctx, ReviseNoteInput{NoteID: note.ID, Body: "must wait", Author: "tester"}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ReviseNote() error = %v, want validation", err)
	}
	pending, err := svc.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if len(pending) != 1 || pending[0].RevisionID != note.RevisionID || pending[0].Body != note.Body {
		t.Fatalf("pending note after rejected revise = %+v", pending)
	}
}

func TestUpdateMovesBindingOnlyForUniqueStructuralMatch(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"a/example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(first) error = %v", err)
	}
	note, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "a/example.Run", Kind: "intent", Body: "moved intent", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(ctx, CommitInput{Message: "document move", Author: "tester"}); err != nil {
		t.Fatalf("Commit(first) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "b"), 0o755); err != nil {
		t.Fatalf("MkdirAll(b): %v", err)
	}
	if err := os.Rename(filepath.Join(repo, "a", "example.go"), filepath.Join(repo, "b", "example.go")); err != nil {
		t.Fatalf("Rename(source): %v", err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "move run")
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(moved) error = %v", err)
	}
	pending, err := svc.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != note.ID || pending[0].EntityKey != "b/example.Run" || pending[0].BindingState != domain.NoteBindingActive {
		t.Fatalf("pending move reconciliation = %+v", pending)
	}
	contextResult, err := svc.Context(ctx, "b/example.Run")
	if err != nil {
		t.Fatalf("Context(moved) error = %v", err)
	}
	if len(contextResult.Notes) != 1 || contextResult.Notes[0].Body != note.Body {
		t.Fatalf("Context(moved) notes = %+v", contextResult.Notes)
	}
}

func TestUpdateMarksAmbiguousOrRemovedBindingNonCurrent(t *testing.T) {
	tests := []struct {
		name       string
		nextFiles  map[string]string
		wantState  domain.NoteBindingState
		wantReason string
	}{
		{
			name: "ambiguous structural matches",
			nextFiles: map[string]string{
				"b/example.go": "package example\nfunc Run() {}\n",
				"c/example.go": "package example\nfunc Run() {}\n",
			},
			wantState:  domain.NoteBindingNeedsReview,
			wantReason: "ambiguous_lineage",
		},
		{
			name:       "removed entity",
			nextFiles:  map[string]string{},
			wantState:  domain.NoteBindingHistorical,
			wantReason: "entity_removed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := newGitRepo(t, map[string]string{"a/example.go": "package example\nfunc Run() {}\n"})
			ctx := context.Background()
			svc, err := Open(ctx, repo)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer svc.Close()
			if err := svc.Init(ctx); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if _, err := svc.Update(ctx); err != nil {
				t.Fatalf("Update(first) error = %v", err)
			}
			note, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "a/example.Run", Kind: "warning", Body: "lineage", Author: "tester"})
			if err != nil {
				t.Fatalf("AddNote() error = %v", err)
			}
			if _, err := svc.Commit(ctx, CommitInput{Message: "document lineage", Author: "tester"}); err != nil {
				t.Fatalf("Commit(first) error = %v", err)
			}
			if err := os.Remove(filepath.Join(repo, "a", "example.go")); err != nil {
				t.Fatalf("Remove(source): %v", err)
			}
			for path, contents := range test.nextFiles {
				writeFile(t, filepath.Join(repo, path), contents)
			}
			git(t, repo, "add", "-A")
			git(t, repo, "commit", "-m", "change lineage")
			if _, err := svc.Update(ctx); err != nil {
				t.Fatalf("Update(changed) error = %v", err)
			}
			pending, err := svc.Diff(ctx)
			if err != nil {
				t.Fatalf("Diff() error = %v", err)
			}
			if len(pending) != 1 || pending[0].ID != note.ID || pending[0].BindingState != test.wantState || pending[0].ReviewReason != test.wantReason {
				t.Fatalf("pending lineage reconciliation = %+v", pending)
			}
			for entityKey := range test.nextFiles {
				key := strings.TrimSuffix(entityKey, ".go") + ".Run"
				contextResult, err := svc.Context(ctx, key)
				if err != nil {
					t.Fatalf("Context(%q) error = %v", key, err)
				}
				if len(contextResult.Notes) != 0 {
					t.Fatalf("Context(%q) notes = %+v, want non-current note hidden", key, contextResult.Notes)
				}
			}
		})
	}
}

func TestCommitRejectsStaleWorkingSet(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"example.go": "package example\nfunc Run() {}\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "run work", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}

	writeFile(t, filepath.Join(repo, "next.go"), "package example\nfunc Next() {}\n")
	git(t, repo, "add", "next.go")
	git(t, repo, "commit", "-m", "advance source")

	_, err = svc.Commit(context.Background(), CommitInput{Message: "stale", Author: "tester"})
	if domain.CodeOf(err) != domain.CodeStaleWorkingSet {
		t.Fatalf("Commit() error = %v, want stale working set", err)
	}
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.PendingNotes != 1 {
		t.Fatalf("PendingNotes = %d, want 1", status.PendingNotes)
	}
}

func TestCommitRejectsGitHeadChangeBeforeFinalization(t *testing.T) {
	for _, test := range []struct {
		name        string
		stableCalls int
	}{
		{name: "before object write", stableCalls: 1},
		{name: "after object write", stableCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
			svc, err := Open(context.Background(), repo)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer svc.Close()
			if err := svc.Init(context.Background()); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if _, err := svc.Update(context.Background()); err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if _, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "keep this pending", Author: "tester"}); err != nil {
				t.Fatalf("AddNote() error = %v", err)
			}

			originalDiscover := svc.discover
			discoverCalls := 0
			svc.discover = func(ctx context.Context, workingDirectory string) (gitrepo.State, error) {
				state, err := originalDiscover(ctx, workingDirectory)
				discoverCalls++
				if err == nil && discoverCalls > test.stableCalls {
					state.HeadSHA = "changed-head"
				}
				return state, err
			}
			if _, err := svc.Commit(context.Background(), CommitInput{Message: "must stay pending", Author: "tester"}); domain.CodeOf(err) != domain.CodeStaleWorkingSet {
				t.Fatalf("Commit() error = %v, want stale working set", err)
			}

			svc.discover = originalDiscover
			status, err := svc.Status(context.Background())
			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if status.PendingNotes != 1 || status.ContextCommitID != "" {
				t.Fatalf("status after changed HEAD = %+v, want pending note preserved", status)
			}
		})
	}
}

func TestCommitPreservesPendingNotesWhenObjectWriteFails(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "keep this note", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	objectDir := localObjectDir(repo)
	if err := os.RemoveAll(objectDir); err != nil {
		t.Fatalf("remove object directory: %v", err)
	}
	if err := os.WriteFile(objectDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("replace object directory with file: %v", err)
	}

	if _, err := svc.Commit(context.Background(), CommitInput{Message: "must fail", Author: "tester"}); domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("Commit() error = %v, want local storage", err)
	}
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.PendingNotes != 1 || status.ContextCommitID != "" {
		t.Fatalf("status after object failure = %+v", status)
	}
}

func TestUpdateRequiresInit(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if _, err := svc.Update(context.Background()); domain.CodeOf(err) != domain.CodeNotInitialized {
		t.Fatalf("Update() error = %v, want not initialized", err)
	}
	if _, err := os.Stat(localDatabasePath(repo)); !os.IsNotExist(err) {
		t.Fatalf("database exists before init: %v", err)
	}
}

func TestUpdateRejectsDirtyWorktree(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() { println(\"dirty\") }\n")
	if _, err := svc.Update(context.Background()); domain.CodeOf(err) != domain.CodeRepositoryState {
		t.Fatalf("Update() error = %v, want repository state", err)
	}
}

func TestUpdateRejectsDetachedHEAD(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	git(t, repo, "checkout", "--detach")
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); domain.CodeOf(err) != domain.CodeRepositoryState {
		t.Fatalf("Update() error = %v, want repository state", err)
	}
}

func TestUpdateExcludesPreviousProjectionWhenCurrentSourceDoesNotParse(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); err != nil {
		t.Fatalf("initial Update() error = %v", err)
	}
	writeFile(t, filepath.Join(repo, "broken.go"), "package example\nfunc Broken( {\n")
	git(t, repo, "add", "broken.go")
	git(t, repo, "commit", "-m", "add broken source")
	if _, err := svc.Update(context.Background()); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Update() error = %v, want validation", err)
	}
	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.EntityCount != 0 || status.CoverageComplete || len(status.Coverage) != 1 || status.Coverage[0].State != domain.CoverageFailed {
		t.Fatalf("status after parse error = %+v, want failed coverage and no current entities", status)
	}
}

func TestAddNoteDefaultsToGitUserName(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	note, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "run work"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if note.Author != "Thread Keep Test" {
		t.Fatalf("note author = %q, want local Git user", note.Author)
	}
}

func TestAddNoteTrimsEntityKey(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	note, err := svc.AddNote(context.Background(), AddNoteInput{EntityKey: "  example.Run  ", Kind: "intent", Body: "trim keys", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if note.EntityKey != "example.Run" {
		t.Fatalf("EntityKey = %q, want trimmed key", note.EntityKey)
	}
}

func TestUpdateSeparatesSamePackageNamesByDirectory(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"a/main.go": "package main\nfunc Run() {}\n",
		"b/main.go": "package main\nfunc Run() {}\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	for _, key := range []string{"a/main.Run", "b/main.Run"} {
		if _, err := svc.Context(context.Background(), key); err != nil {
			t.Fatalf("Context(%q) error = %v", key, err)
		}
	}
}

func TestUpdateSeparatesDottedAndNestedDirectories(t *testing.T) {
	t.Parallel()
	repo := newGitRepo(t, map[string]string{
		"a.b/main.go": "package main\nfunc Run() {}\n",
		"a/b/main.go": "package main\nfunc Run() {}\n",
	})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(context.Background()); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	for _, key := range []string{"a.b/main.Run", "a/b/main.Run"} {
		if _, err := svc.Context(context.Background(), key); err != nil {
			t.Fatalf("Context(%q) error = %v", key, err)
		}
	}
}

func TestRelatedContextReturnsBoundedStructuralEvidence(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"worker.go": `package example

type Worker struct{}

func (w *Worker) Run() {}

func Helper() {}
`,
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	related, err := svc.RelatedContext(ctx, "example.Worker.Run", 20)
	if err != nil {
		t.Fatalf("RelatedContext() error = %v", err)
	}
	if len(related) != 2 {
		t.Fatalf("RelatedContext() = %+v, want owner and same-file helper", related)
	}
	if related[0].EntityKey != "example.Worker" || related[0].EdgeKind != "method_owner" || !related[0].Fresh {
		t.Fatalf("owner relation = %+v", related[0])
	}
	if related[1].EntityKey != "example.Helper" || related[1].EdgeKind != "same_file" || !related[1].Fresh {
		t.Fatalf("same-file relation = %+v", related[1])
	}
	if _, err := svc.RelatedContext(ctx, "example.Worker.Run", 0); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("RelatedContext(limit=0) error = %v, want validation", err)
	}
	if _, err := svc.RelatedContext(ctx, "missing.Entity", 20); domain.CodeOf(err) != domain.CodeEntityNotFound {
		t.Fatalf("RelatedContext(missing) error = %v, want entity not found", err)
	}
}

func TestRelatedContextUsesExternalIndexerOwnerKeys(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"worker.ts": "class Worker { run(): void {} }\n",
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	svc.indexer = indexCoordinatorFunc(func(_ context.Context, _ string, sourceSHA string) ([]domain.LanguageProjection, error) {
		return []domain.LanguageProjection{{
			Coverage: domain.Coverage{Language: "typescript", State: domain.CoverageIndexed, IndexerID: "thread-keep-index-typescript", IndexerVersion: "1", SourceSHA: sourceSHA},
			Entities: []domain.Entity{
				{Language: "typescript", Key: "typescript:worker.ts#class:Worker", Kind: domain.EntityClass, Name: "Worker", Path: "worker.ts", SourceSHA: sourceSHA, StructuralHash: "worker"},
				{Language: "typescript", Key: "typescript:worker.ts#method:Worker.run", Kind: domain.EntityMethod, Name: "run", Path: "worker.ts", SourceSHA: sourceSHA, StructuralHash: "run"},
			},
		}}, nil
	})
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	related, err := svc.RelatedContext(ctx, "typescript:worker.ts#method:Worker.run", 20)
	if err != nil {
		t.Fatalf("RelatedContext() error = %v", err)
	}
	if len(related) != 1 || related[0].EntityKey != "typescript:worker.ts#class:Worker" || related[0].EdgeKind != "method_owner" {
		t.Fatalf("RelatedContext() = %+v, want external indexer method owner", related)
	}
}

func TestSearchRanksExactNameBeforeActiveNoteEvidence(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"example.go": `package example

func Target() {}

func Other() {}
`,
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Other", Kind: "example", Body: "Target context", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	hits, err := svc.Search(ctx, "Target")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 2 || hits[0].EntityKey != "example.Target" || !containsMatchField(hits[0].MatchedFields, domain.SearchMatchName) || hits[1].EntityKey != "example.Other" || !containsMatchField(hits[1].MatchedFields, domain.SearchMatchNoteBody) {
		t.Fatalf("Search() = %+v, want exact name before note evidence", hits)
	}
}

func TestSearchReportsEvidenceForPunctuationNormalizedFTSMatch(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"example.go": "package example\n\nfunc Run() {}\n",
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	hits, err := svc.Search(ctx, "Run()")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 1 || hits[0].EntityKey != "example.Run" || !containsMatchField(hits[0].MatchedFields, domain.SearchMatchName) || len(hits[0].MatchedTerms) != 1 || hits[0].MatchedTerms[0] != "Run()" {
		t.Fatalf("Search() = %+v, want name evidence for punctuation-normalized FTS match", hits)
	}
}

func TestSearchAndRelatedContextAreReadOnly(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"worker.go": `package example

type Worker struct{}

func (w *Worker) Run() {}
`,
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	before, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status(before) error = %v", err)
	}
	if _, err := svc.Search(ctx, "Worker"); err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if _, err := svc.RelatedContext(ctx, "example.Worker.Run", 20); err != nil {
		t.Fatalf("RelatedContext() error = %v", err)
	}
	after, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status(after) error = %v", err)
	}
	if before.PendingNotes != after.PendingNotes || before.ContextCommitID != after.ContextCommitID || before.SourceSHA != after.SourceSHA || before.EntityCount != after.EntityCount {
		t.Fatalf("search changed status: before=%+v after=%+v", before, after)
	}
}

func TestSearchAndRelatedContextRejectStaleWorkingSet(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"worker.go": "package example\n\ntype Worker struct{}\n\nfunc (w *Worker) Run() {}\n",
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	writeFile(t, filepath.Join(repo, "worker.go"), "package example\n\ntype Worker struct{}\n\nfunc (w *Worker) Run() {}\n\nfunc Helper() {}\n")
	git(t, repo, "add", "worker.go")
	git(t, repo, "commit", "-m", "advance source")
	if _, err := svc.Search(ctx, "Worker"); domain.CodeOf(err) != domain.CodeStaleWorkingSet {
		t.Fatalf("Search() error = %v, want stale working set", err)
	}
	if _, err := svc.RelatedContext(ctx, "example.Worker.Run", 20); domain.CodeOf(err) != domain.CodeStaleWorkingSet {
		t.Fatalf("RelatedContext() error = %v, want stale working set", err)
	}
	if _, err := svc.AssembleContext(ctx, domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: "example.Worker.Run"}}); domain.CodeOf(err) != domain.CodeStaleWorkingSet {
		t.Fatalf("AssembleContext() error = %v, want stale working set", err)
	}
}

func TestAssembleContextReturnsDirectAndLexicalEvidence(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"worker.go": `package example

type Worker struct{}

func (w *Worker) Run() {}

func Helper() {}
`,
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	direct, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Worker.Run", Kind: "intent", Body: "run the worker", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote(direct) error = %v", err)
	}
	owner, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Worker", Kind: "constraint", Body: "worker service constraint", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote(owner) error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Helper", Kind: "warning", Body: "unrelated helper warning", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(helper) error = %v", err)
	}

	entityBundle, err := svc.AssembleContext(ctx, domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: "example.Worker.Run"}})
	if err != nil {
		t.Fatalf("AssembleContext(entity) error = %v", err)
	}
	if len(entityBundle.Items) != 1 || entityBundle.Items[0].Note.ID != direct.ID {
		t.Fatalf("AssembleContext(entity) = %+v, want only directly bound note", entityBundle.Items)
	}

	textBundle, err := svc.AssembleContext(ctx, domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorText, Query: "service constraint"}})
	if err != nil {
		t.Fatalf("AssembleContext(text) error = %v", err)
	}
	if len(textBundle.Items) != 1 || textBundle.Items[0].Note.ID != owner.ID || textBundle.Items[0].Reasons[0].Kind != domain.ReasonTextNoteBody {
		t.Fatalf("AssembleContext(text) = %+v, want lexical note evidence", textBundle.Items)
	}

	empty, err := svc.AssembleContext(ctx, domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorText, Query: "not-present"}})
	if err != nil {
		t.Fatalf("AssembleContext(empty text) error = %v", err)
	}
	if len(empty.Items) != 0 {
		t.Fatalf("AssembleContext(empty text) = %+v, want no fabricated evidence", empty.Items)
	}
}

func TestAssembleContextForChangeUsesContextTipEntityLineage(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"modified.go": "package example\n\nfunc Modified() {}\n",
		"removed.go":  "package example\n\nfunc Removed() {}\n",
		"old/move.go": "package example\n\nfunc Moved() {}\n",
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(base) error = %v", err)
	}
	modified, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Modified", Kind: "constraint", Body: "modified constraint", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote(modified) error = %v", err)
	}
	moved, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "old/example.Moved", Kind: "decision", Body: "moved decision", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote(moved) error = %v", err)
	}
	removed, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Removed", Kind: "warning", Body: "removed warning", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote(removed) error = %v", err)
	}
	baseCommit, err := svc.Commit(ctx, CommitInput{Message: "base context", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(base) error = %v", err)
	}

	writeFile(t, filepath.Join(repo, "modified.go"), "package example\n\nfunc Modified() { println(\"changed\") }\n")
	writeFile(t, filepath.Join(repo, "added.go"), "package example\n\nfunc Added() {}\n")
	if err := os.MkdirAll(filepath.Join(repo, "new"), 0o755); err != nil {
		t.Fatalf("MkdirAll(new) error = %v", err)
	}
	if err := os.Rename(filepath.Join(repo, "old", "move.go"), filepath.Join(repo, "new", "move.go")); err != nil {
		t.Fatalf("Rename(move.go) error = %v", err)
	}
	if err := os.Remove(filepath.Join(repo, "removed.go")); err != nil {
		t.Fatalf("Remove(removed.go) error = %v", err)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "change entities")
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(current) error = %v", err)
	}

	bundle, err := svc.AssembleContext(ctx, domain.ContextQuery{
		Anchor: domain.ContextAnchor{Kind: domain.AnchorChange},
		States: []domain.NoteBindingState{domain.NoteBindingActive, domain.NoteBindingNeedsReview, domain.NoteBindingHistorical},
	})
	if err != nil {
		t.Fatalf("AssembleContext(change) error = %v", err)
	}
	if bundle.Source.ContextCommitID != baseCommit.ID || bundle.Source.BaseSourceSHA != baseCommit.SourceSHA {
		t.Fatalf("change source = %+v, want base context tip", bundle.Source)
	}
	if len(bundle.Anchor.Changes) != 4 {
		t.Fatalf("change anchor = %+v, want added, modified, moved, removed", bundle.Anchor)
	}
	if len(bundle.Items) != 3 {
		t.Fatalf("change items = %+v, want notes for modified, moved, removed roots", bundle.Items)
	}
	states := map[string]domain.NoteBindingState{}
	for _, item := range bundle.Items {
		states[item.Note.ID] = item.Note.BindingState
		if item.Reasons[0].Kind != domain.ReasonChangedEntity {
			t.Fatalf("change item reason = %+v", item)
		}
	}
	if states[modified.ID] != domain.NoteBindingNeedsReview || states[moved.ID] != domain.NoteBindingActive || states[removed.ID] != domain.NoteBindingHistorical {
		t.Fatalf("change item states = %+v", states)
	}
}

func TestAssembleContextForChangeRejectsMissingAndNonAncestorBases(t *testing.T) {
	t.Run("missing context tip", func(t *testing.T) {
		repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
		ctx := context.Background()
		svc, err := Open(ctx, repo)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		defer svc.Close()
		if err := svc.Init(ctx); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		if _, err := svc.Update(ctx); err != nil {
			t.Fatalf("Update() error = %v", err)
		}
		if _, err := svc.AssembleContext(ctx, domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorChange}}); domain.CodeOf(err) != domain.CodeValidation {
			t.Fatalf("AssembleContext() error = %v, want missing-base validation", err)
		}
	})

	t.Run("non ancestor source", func(t *testing.T) {
		repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
		ctx := context.Background()
		svc, err := Open(ctx, repo)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		defer svc.Close()
		if err := svc.Init(ctx); err != nil {
			t.Fatalf("Init() error = %v", err)
		}
		if _, err := svc.Update(ctx); err != nil {
			t.Fatalf("Update(base) error = %v", err)
		}
		if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "base", Author: "tester"}); err != nil {
			t.Fatalf("AddNote() error = %v", err)
		}
		base, err := svc.Commit(ctx, CommitInput{Message: "base context", Author: "tester"})
		if err != nil {
			t.Fatalf("Commit(base) error = %v", err)
		}
		writeFile(t, filepath.Join(repo, "example.go"), "package example\n\nfunc Run() { println(1) }\n")
		git(t, repo, "add", "example.go")
		git(t, repo, "commit", "-m", "source branch one")
		if _, err := svc.Update(ctx); err != nil {
			t.Fatalf("Update(branch one) error = %v", err)
		}
		if _, err := svc.Commit(ctx, CommitInput{Message: "branch one context", Author: "tester"}); err != nil {
			t.Fatalf("Commit(branch one) error = %v", err)
		}
		git(t, repo, "reset", "--hard", base.SourceSHA)
		writeFile(t, filepath.Join(repo, "example.go"), "package example\n\nfunc Run() { println(2) }\n")
		git(t, repo, "add", "example.go")
		git(t, repo, "commit", "-m", "source branch two")
		if _, err := svc.Update(ctx); err != nil {
			t.Fatalf("Update(branch two) error = %v", err)
		}
		if _, err := svc.AssembleContext(ctx, domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorChange}}); domain.CodeOf(err) != domain.CodeValidation {
			t.Fatalf("AssembleContext(non-ancestor) error = %v, want validation", err)
		}
	})
}

func TestAssembleContextReportsIgnoredDirtyWorktree(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "committed source only", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	writeFile(t, filepath.Join(repo, "scratch.txt"), "dirty")
	bundle, err := svc.AssembleContext(ctx, domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: "example.Run"}})
	if err != nil {
		t.Fatalf("AssembleContext() error = %v", err)
	}
	if len(bundle.Diagnostics) != 1 || bundle.Diagnostics[0].Code != "uncommitted_changes_ignored" {
		t.Fatalf("AssembleContext() diagnostics = %+v", bundle.Diagnostics)
	}
}

func TestAssembleContextDiscardsResultWhenSourceChangesDuringRead(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	original := svc.discover
	var calls int
	svc.discover = func(ctx context.Context, root string) (gitrepo.State, error) {
		state, err := original(ctx, root)
		calls++
		if calls > 1 {
			state.HeadSHA = strings.Repeat("f", 40)
		}
		return state, err
	}
	if _, err := svc.AssembleContext(ctx, domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: "example.Run"}}); domain.CodeOf(err) != domain.CodeStaleWorkingSet {
		t.Fatalf("AssembleContext() error = %v, want stale_working_set", err)
	}
}

func TestAssembleContextHistoryReturnsSupersededRevisionWithoutMakingItCurrent(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	first, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "decision", Body: "first historical decision", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	firstCommit, err := svc.Commit(ctx, CommitInput{Message: "first decision", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(first) error = %v", err)
	}
	current, err := svc.ReviseNote(ctx, ReviseNoteInput{NoteID: first.ID, Body: "current decision", Author: "tester"})
	if err != nil {
		t.Fatalf("ReviseNote() error = %v", err)
	}
	if _, err := svc.Commit(ctx, CommitInput{Message: "current decision", Author: "tester"}); err != nil {
		t.Fatalf("Commit(current) error = %v", err)
	}

	currentOnly, err := svc.AssembleContext(ctx, domain.ContextQuery{Anchor: domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: "example.Run"}})
	if err != nil {
		t.Fatalf("AssembleContext(current) error = %v", err)
	}
	if len(currentOnly.Items) != 1 || currentOnly.Items[0].Note.RevisionID != current.RevisionID || currentOnly.Items[0].Historical {
		t.Fatalf("current items = %+v", currentOnly.Items)
	}

	all, err := svc.AssembleContext(ctx, domain.ContextQuery{
		Anchor:  domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: "example.Run"},
		History: domain.HistoryAll,
	})
	if err != nil {
		t.Fatalf("AssembleContext(history) error = %v", err)
	}
	if len(all.Items) != 2 || all.Items[0].Note.RevisionID != current.RevisionID || all.Items[0].Historical || all.Items[1].Note.RevisionID != first.RevisionID || !all.Items[1].Historical || all.Items[1].ContextCommit != firstCommit.ID {
		t.Fatalf("history items = %+v, want current then first historical revision", all.Items)
	}
	if all.Items[1].Reasons[len(all.Items[1].Reasons)-1].Kind != domain.ReasonHistoricalRevision {
		t.Fatalf("historical reasons = %+v", all.Items[1].Reasons)
	}

	text, err := svc.AssembleContext(ctx, domain.ContextQuery{
		Anchor:  domain.ContextAnchor{Kind: domain.AnchorText, Query: "first historical"},
		History: domain.HistoryAll,
	})
	if err != nil {
		t.Fatalf("AssembleContext(history text) error = %v", err)
	}
	if len(text.Items) != 1 || text.Items[0].Note.RevisionID != first.RevisionID || !text.Items[0].Historical {
		t.Fatalf("historical text items = %+v", text.Items)
	}
}

func TestAssembleContextHistoryRejectsMissingImmutableAncestry(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "decision", Body: "history integrity", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	commit, err := svc.Commit(ctx, CommitInput{Message: "history", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := os.Remove(localObjectPath(repo, commit.ID)); err != nil {
		t.Fatalf("Remove(context object) error = %v", err)
	}
	if _, err := svc.AssembleContext(ctx, domain.ContextQuery{
		Anchor:  domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: "example.Run"},
		History: domain.HistoryAll,
	}); domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("AssembleContext(missing history) error = %v, want local_storage", err)
	}
}

func TestNoteTopicsRoundTripThroughAssemblyAndRevision(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"worker.go": "package example\n\ntype Worker struct{}\n\nfunc (w *Worker) Run() {}\n",
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	tagged, err := svc.AddNote(ctx, AddNoteInput{
		EntityKey: "example.Worker", Kind: "constraint", Body: "evict entries safely", Author: "tester",
		Topics: []string{" Cache-Invalidation "},
	})
	if err != nil {
		t.Fatalf("AddNote(tagged) error = %v", err)
	}
	if !reflect.DeepEqual(tagged.Topics, []string{"cache-invalidation"}) {
		t.Fatalf("tagged note = %+v", tagged)
	}

	topic, err := svc.AssembleContext(ctx, domain.ContextQuery{
		Anchor: domain.ContextAnchor{Kind: domain.AnchorText, Query: "cache-invalidation"},
		Topics: []string{"cache-invalidation"},
	})
	if err != nil {
		t.Fatalf("AssembleContext(topic) error = %v", err)
	}
	if len(topic.Items) != 1 || topic.Items[0].Note.ID != tagged.ID || topic.Items[0].Reasons[0].Kind != domain.ReasonTopicExact {
		t.Fatalf("topic items = %+v", topic.Items)
	}

	if _, err := svc.Commit(ctx, CommitInput{Message: "topic context", Author: "tester"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	revised, err := svc.ReviseNote(ctx, ReviseNoteInput{
		NoteID: tagged.ID, Body: "revised eviction rule", Author: "reviewer", Topics: []string{"cache-eviction"},
	})
	if err != nil {
		t.Fatalf("ReviseNote() error = %v", err)
	}
	if revised.SupersedesRevisionID != tagged.RevisionID || !reflect.DeepEqual(revised.Topics, []string{"cache-eviction"}) {
		t.Fatalf("revised note = %+v", revised)
	}
}

func TestRebuildRestoresNoteTopics(t *testing.T) {
	repo := newGitRepo(t, map[string]string{
		"worker.go": "package example\n\ntype Worker struct{}\n\nfunc (w *Worker) Run() {}\n",
	})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{
		EntityKey: "example.Worker", Kind: "constraint", Body: "restored topic rule", Author: "tester", Topics: []string{"rebuild-topic"},
	}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	commit, err := svc.Commit(ctx, CommitInput{Message: "metadata snapshot", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := os.Remove(localDatabasePath(repo)); err != nil {
		t.Fatalf("Remove(projection) error = %v", err)
	}
	svc, err = Open(ctx, repo)
	if err != nil {
		t.Fatalf("Reopen() error = %v", err)
	}
	defer svc.Close()
	if _, err := svc.Rebuild(ctx, commit.ID); err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	result, err := svc.Context(ctx, "example.Worker")
	if err != nil || len(result.Notes) != 1 || !reflect.DeepEqual(result.Notes[0].Topics, []string{"rebuild-topic"}) {
		t.Fatalf("Context() after rebuild = %+v, %v", result, err)
	}
}

func TestRemotePushAndFastForwardPullPreserveImmutableContext(t *testing.T) {
	ctx := context.Background()
	primaryRepo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	secondaryRepo := filepath.Join(t.TempDir(), "secondary")
	git(t, primaryRepo, "clone", "--no-local", primaryRepo, secondaryRepo)
	remotePath := t.TempDir()

	primary, err := Open(ctx, primaryRepo)
	if err != nil {
		t.Fatalf("Open(primary) error = %v", err)
	}
	defer primary.Close()
	if err := primary.Init(ctx); err != nil {
		t.Fatalf("Init(primary) error = %v", err)
	}
	if _, err := primary.Update(ctx); err != nil {
		t.Fatalf("Update(primary) error = %v", err)
	}
	if _, err := primary.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "share immutable context", Author: "tester", Topics: []string{"remote-context"}}); err != nil {
		t.Fatalf("AddNote(primary) error = %v", err)
	}
	if _, err := primary.Commit(ctx, CommitInput{Message: "share context", Author: "tester"}); err != nil {
		t.Fatalf("Commit(primary) error = %v", err)
	}
	if _, err := primary.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote(primary) error = %v", err)
	}
	pushed, err := primary.PushRemote(ctx, "origin")
	if err != nil || pushed.Outcome != "pushed" || pushed.TransferredObjects != 1 {
		t.Fatalf("PushRemote() = %+v, %v", pushed, err)
	}

	secondary, err := Open(ctx, secondaryRepo)
	if err != nil {
		t.Fatalf("Open(secondary) error = %v", err)
	}
	defer secondary.Close()
	if err := secondary.Init(ctx); err != nil {
		t.Fatalf("Init(secondary) error = %v", err)
	}
	if _, err := secondary.Update(ctx); err != nil {
		t.Fatalf("Update(secondary) error = %v", err)
	}
	if _, err := secondary.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote(secondary) error = %v", err)
	}
	pulled, err := secondary.PullRemote(ctx, "origin")
	if err != nil || pulled.Outcome != "pulled" || pulled.TransferredObjects != 1 {
		t.Fatalf("PullRemote() = %+v, %v", pulled, err)
	}
	contextResult, err := secondary.Context(ctx, "example.Run")
	if err != nil || len(contextResult.Notes) != 1 || contextResult.Notes[0].Body != "share immutable context" || !reflect.DeepEqual(contextResult.Notes[0].Topics, []string{"remote-context"}) {
		t.Fatalf("Context() after pull = %+v, %v", contextResult, err)
	}
	upToDate, err := secondary.PullRemote(ctx, "origin")
	if err != nil || upToDate.Outcome != "up_to_date" {
		t.Fatalf("PullRemote(up to date) = %+v, %v", upToDate, err)
	}
}

func TestPullRemoteRejectsPendingContextBeforeRemoteIO(t *testing.T) {
	ctx := context.Background()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	remotePath := t.TempDir()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "must remain pending", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.PullRemote(ctx, "origin"); domain.CodeOf(err) != domain.CodeWorkingSetDirty {
		t.Fatalf("PullRemote() error = %v, want working set dirty", err)
	}
	entries, err := os.ReadDir(remotePath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("remote path after rejected pull = %+v, %v; want empty, nil", entries, err)
	}
}

func TestPushRemoteRejectsRemoteAheadWithoutOverwritingIt(t *testing.T) {
	ctx := context.Background()
	primaryRepo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	secondaryRepo := filepath.Join(t.TempDir(), "secondary")
	git(t, primaryRepo, "clone", "--no-local", primaryRepo, secondaryRepo)
	remotePath := t.TempDir()

	primary, err := Open(ctx, primaryRepo)
	if err != nil {
		t.Fatalf("Open(primary) error = %v", err)
	}
	defer primary.Close()
	if err := primary.Init(ctx); err != nil {
		t.Fatalf("Init(primary) error = %v", err)
	}
	if _, err := primary.Update(ctx); err != nil {
		t.Fatalf("Update(primary) error = %v", err)
	}
	if _, err := primary.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "remote winner", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(primary) error = %v", err)
	}
	if _, err := primary.Commit(ctx, CommitInput{Message: "primary", Author: "tester"}); err != nil {
		t.Fatalf("Commit(primary) error = %v", err)
	}
	if _, err := primary.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote(primary) error = %v", err)
	}
	if _, err := primary.PushRemote(ctx, "origin"); err != nil {
		t.Fatalf("PushRemote(primary) error = %v", err)
	}

	secondary, err := Open(ctx, secondaryRepo)
	if err != nil {
		t.Fatalf("Open(secondary) error = %v", err)
	}
	defer secondary.Close()
	if err := secondary.Init(ctx); err != nil {
		t.Fatalf("Init(secondary) error = %v", err)
	}
	if _, err := secondary.Update(ctx); err != nil {
		t.Fatalf("Update(secondary) error = %v", err)
	}
	if _, err := secondary.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "warning", Body: "local loser", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(secondary) error = %v", err)
	}
	localCommit, err := secondary.Commit(ctx, CommitInput{Message: "secondary", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(secondary) error = %v", err)
	}
	if _, err := secondary.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote(secondary) error = %v", err)
	}
	if _, err := secondary.PushRemote(ctx, "origin"); domain.CodeOf(err) != domain.CodeRemoteConflict {
		t.Fatalf("PushRemote(secondary) error = %v, want remote conflict", err)
	}
	status, err := secondary.Status(ctx)
	if err != nil || status.ContextCommitID != localCommit.ID {
		t.Fatalf("Status(secondary) = %+v, %v; want local commit %s", status, err, localCommit.ID)
	}
}

func TestPushRemoteRejectsInconsistentRemoteRefBeforePublication(t *testing.T) {
	ctx := context.Background()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	remotePath := t.TempDir()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "first", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(ctx, CommitInput{Message: "first", Author: "tester"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, err := svc.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote() error = %v", err)
	}
	if _, err := svc.PushRemote(ctx, "origin"); err != nil {
		t.Fatalf("PushRemote(first) error = %v", err)
	}
	transport, err := remote.Open(remotePath)
	if err != nil {
		t.Fatalf("remote.Open() error = %v", err)
	}
	refName := "refs/contexts/main"
	current, err := transport.ReadRef(ctx, refName)
	if err != nil {
		t.Fatalf("ReadRef() error = %v", err)
	}
	if _, err := transport.CompareAndSwapRef(ctx, refName, current, remote.Ref{RefName: refName, CommitID: current.CommitID, SourceSHA: "wrong-source", Version: current.Version + 1}); err != nil {
		t.Fatalf("corrupt remote ref: %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "warning", Body: "second", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(second) error = %v", err)
	}
	local, err := svc.Commit(ctx, CommitInput{Message: "second", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(second) error = %v", err)
	}
	if _, err := svc.PushRemote(ctx, "origin"); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("PushRemote(inconsistent ref) error = %v, want validation", err)
	}
	if _, err := transport.ReadObject(ctx, local.ID); domain.CodeOf(err) != domain.CodeObjectMissing {
		t.Fatalf("ReadObject(unpublished local tip) error = %v, want object missing", err)
	}
}

func TestFetchRemoteAllowsPendingContextAndPullRejectsSourceMismatch(t *testing.T) {
	ctx := context.Background()
	primaryRepo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	fetchRepo := filepath.Join(t.TempDir(), "fetch")
	staleRepo := filepath.Join(t.TempDir(), "stale")
	git(t, primaryRepo, "clone", "--no-local", primaryRepo, fetchRepo)
	git(t, primaryRepo, "clone", "--no-local", primaryRepo, staleRepo)
	git(t, staleRepo, "config", "user.name", "Thread Keep Test")
	git(t, staleRepo, "config", "user.email", "thread-keep@example.test")
	remotePath := t.TempDir()

	primary, err := Open(ctx, primaryRepo)
	if err != nil {
		t.Fatalf("Open(primary) error = %v", err)
	}
	defer primary.Close()
	if err := primary.Init(ctx); err != nil {
		t.Fatalf("Init(primary) error = %v", err)
	}
	if _, err := primary.Update(ctx); err != nil {
		t.Fatalf("Update(primary) error = %v", err)
	}
	if _, err := primary.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "remote context", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(primary) error = %v", err)
	}
	if _, err := primary.Commit(ctx, CommitInput{Message: "primary", Author: "tester"}); err != nil {
		t.Fatalf("Commit(primary) error = %v", err)
	}
	if _, err := primary.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote(primary) error = %v", err)
	}
	if _, err := primary.PushRemote(ctx, "origin"); err != nil {
		t.Fatalf("PushRemote(primary) error = %v", err)
	}

	fetcher, err := Open(ctx, fetchRepo)
	if err != nil {
		t.Fatalf("Open(fetch) error = %v", err)
	}
	defer fetcher.Close()
	if err := fetcher.Init(ctx); err != nil {
		t.Fatalf("Init(fetch) error = %v", err)
	}
	if _, err := fetcher.Update(ctx); err != nil {
		t.Fatalf("Update(fetch) error = %v", err)
	}
	if _, err := fetcher.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote(fetch) error = %v", err)
	}
	if _, err := fetcher.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "warning", Body: "remain pending", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(fetch) error = %v", err)
	}
	fetched, err := fetcher.FetchRemote(ctx, "origin")
	if err != nil || fetched.Outcome != "fetched" || fetched.TransferredObjects != 1 {
		t.Fatalf("FetchRemote() = %+v, %v", fetched, err)
	}
	status, err := fetcher.Status(ctx)
	if err != nil || status.PendingNotes != 1 || status.ContextCommitID != "" {
		t.Fatalf("Status(fetch) = %+v, %v; want pending-only local context", status, err)
	}

	writeFile(t, filepath.Join(staleRepo, "example.go"), "package example\n\nfunc Run() { println(\"new source\") }\n")
	git(t, staleRepo, "add", "example.go")
	git(t, staleRepo, "commit", "-m", "change source")
	stale, err := Open(ctx, staleRepo)
	if err != nil {
		t.Fatalf("Open(stale) error = %v", err)
	}
	defer stale.Close()
	if err := stale.Init(ctx); err != nil {
		t.Fatalf("Init(stale) error = %v", err)
	}
	if _, err := stale.Update(ctx); err != nil {
		t.Fatalf("Update(stale) error = %v", err)
	}
	if _, err := stale.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote(stale) error = %v", err)
	}
	if _, err := stale.PullRemote(ctx, "origin"); domain.CodeOf(err) != domain.CodeStaleWorkingSet {
		t.Fatalf("PullRemote(source mismatch) error = %v, want stale working set", err)
	}
	status, err = stale.Status(ctx)
	if err != nil || status.ContextCommitID != "" {
		t.Fatalf("Status(stale) = %+v, %v; want no local context ref", status, err)
	}
}

func TestImportCandidateReadsOnlyExplicitLocalEnvelope(t *testing.T) {
	ctx := context.Background()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "candidate.json")
	contents := fmt.Sprintf(`{"schema_version":1,"provider":"github","repository":"owner/repository","number":42,"state":"open","base_sha":"%s","head_sha":"%s","merge_sha":"","updated_at":"2026-07-12T00:00:00Z","notes":[{"id":"note-1","entity_key":"example.Run","structural_hash":"%s","kind":"intent","body":"candidate body","author":"reviewer","origin":"provider","created_at":"2026-07-12T00:00:00Z"}]}`, strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 64))
	writeFile(t, path, contents)
	result, err := svc.ImportCandidate(ctx, path)
	if err != nil || !result.Imported || result.Candidate.ID != "github:owner/repository#42" || result.DraftNotes != 1 {
		t.Fatalf("ImportCandidate() = %+v, %v", result, err)
	}
}

func TestPromoteMergedCandidateCreatesExplicitPendingOutcomes(t *testing.T) {
	ctx := context.Background()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n\nfunc Changed() {}\n"})
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	run, err := svc.Context(ctx, "example.Run")
	if err != nil {
		t.Fatalf("Context(Run) error = %v", err)
	}
	status, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "candidate.json")
	contents := fmt.Sprintf(`{"schema_version":1,"provider":"github","repository":"owner/repository","number":42,"state":"merged","base_sha":"%s","head_sha":"%s","merge_sha":"%s","updated_at":"2026-07-12T00:00:00Z","notes":[{"id":"exact","entity_key":"example.Run","structural_hash":"%s","kind":"intent","body":"exact candidate","author":"reviewer","origin":"provider","created_at":"2026-07-12T00:00:00Z"},{"id":"changed","entity_key":"example.Changed","structural_hash":"%s","kind":"warning","body":"changed candidate","author":"reviewer","origin":"provider","created_at":"2026-07-12T00:00:00Z"},{"id":"missing","entity_key":"example.Missing","structural_hash":"%s","kind":"example","body":"missing candidate","author":"reviewer","origin":"provider","created_at":"2026-07-12T00:00:00Z"}]}`, status.SourceSHA, status.SourceSHA, status.SourceSHA, run.Entity.StructuralHash, strings.Repeat("a", 64), strings.Repeat("b", 64))
	writeFile(t, path, contents)
	if _, err := svc.ImportCandidate(ctx, path); err != nil {
		t.Fatalf("ImportCandidate() error = %v", err)
	}
	result, err := svc.PromoteCandidate(ctx, "github:owner/repository#42")
	if err != nil || !result.Promoted || result.ActiveNotes != 1 || result.NeedsReviewNotes != 1 || result.HistoricalNotes != 1 {
		t.Fatalf("PromoteCandidate() = %+v, %v", result, err)
	}
	pending, err := svc.Diff(ctx)
	if err != nil || len(pending) != 2 || !containsBindingState(pending, domain.NoteBindingActive) || !containsBindingState(pending, domain.NoteBindingNeedsReview) {
		t.Fatalf("Diff() after promotion = %+v, %v", pending, err)
	}
	repeat, err := svc.PromoteCandidate(ctx, "github:owner/repository#42")
	if err != nil || repeat.Promoted {
		t.Fatalf("repeat PromoteCandidate() = %+v, %v; want no-op", repeat, err)
	}
}

func TestAuthoredMergeRecordUsesDeterministicRevisionID(t *testing.T) {
	base := domain.SnapshotMergeRecord{Note: domain.Note{ID: "note", RevisionID: "base-revision"}}
	session := domain.MergeSession{LocalSnapshotID: strings.Repeat("a", 64), RemoteSnapshotID: strings.Repeat("b", 64), SourceSHA: strings.Repeat("c", 40), PlannedCreatedAt: time.Date(2026, time.July, 12, 0, 0, 0, 0, time.UTC), Conflicts: []domain.MergeSessionConflict{{ID: "conflict", SnapshotMergeConflict: domain.SnapshotMergeConflict{NoteID: "note", Base: &base}}}}
	input := MergeResolveInput{ConflictID: "conflict", Use: "authored", EntityKey: "example.Run", Kind: "decision", Body: "keep merge explicit", Author: "reviewer", Origin: "human"}
	first, err := authoredMergeRecord(session, input)
	if err != nil {
		t.Fatalf("authoredMergeRecord(first) error = %v", err)
	}
	second, err := authoredMergeRecord(session, input)
	if err != nil || first.Note.RevisionID == "" || first.Note.RevisionID != second.Note.RevisionID || first.Note.SupersedesRevisionID != "base-revision" || first.Note.CreatedAt != session.PlannedCreatedAt || first.Mapping.NoteID != "note" || first.Mapping.RevisionID != first.Note.RevisionID {
		t.Fatalf("authoredMergeRecord() = %+v, %+v, %v", first, second, err)
	}
}

func TestStartMergeRejectsIdenticalSnapshotInputs(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "merge baseline", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	commit, err := svc.Commit(ctx, CommitInput{Message: "baseline", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, err := svc.StartMerge(ctx, MergeStartInput{LocalSnapshotID: commit.ID, RemoteSnapshotID: commit.ID, Message: "invalid", Author: "tester"}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("StartMerge() error = %v, want validation", err)
	}
}

func TestCommitMergeCreatesTwoParentSnapshotFromNonOverlappingNotes(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = svc.Close() }()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "base", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(base) error = %v", err)
	}
	base, err := svc.Commit(ctx, CommitInput{Message: "base", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(base) error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "warning", Body: "local", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(local) error = %v", err)
	}
	local, err := svc.Commit(ctx, CommitInput{Message: "local", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(local) error = %v", err)
	}
	baseBytes, err := os.ReadFile(localObjectPath(repo, base.ID))
	if err != nil {
		t.Fatalf("read base snapshot: %v", err)
	}
	var baseObject domain.ContextObject
	if err := json.Unmarshal(baseBytes, &baseObject); err != nil {
		t.Fatalf("decode base snapshot: %v", err)
	}
	remoteNote := domain.Note{ID: "remote-note", RevisionID: "remote-revision", EntityKey: "example.Run", Kind: domain.NoteDecision, Body: "remote", Author: "reviewer", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: base.SourceSHA, Topics: []string{"remote-merge"}}
	remoteObject := domain.ContextObject{SchemaVersion: 3, RepositoryID: baseObject.RepositoryID, RefName: baseObject.RefName, ParentIDs: []string{base.ID}, SourceSHA: baseObject.SourceSHA, Message: "remote", Author: "reviewer", CreatedAt: time.Now().UTC(), Provenance: baseObject.Provenance, Entities: baseObject.Entities, Notes: append(append([]domain.Note(nil), baseObject.Notes...), remoteNote)}
	remoteObject.RevisionMappings = contextRevisionMappings(remoteObject.Notes)
	remoteBytes, err := json.Marshal(remoteObject)
	if err != nil {
		t.Fatalf("marshal remote snapshot: %v", err)
	}
	remoteDigest := blake3.Sum256(remoteBytes)
	remoteID := fmt.Sprintf("%x", remoteDigest[:])
	contextStore, err := svc.openStore(ctx, false)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := contextStore.WriteObject(remoteID, remoteBytes); err != nil {
		t.Fatalf("write remote snapshot: %v", err)
	}
	session, err := svc.StartMerge(ctx, MergeStartInput{LocalSnapshotID: local.ID, RemoteSnapshotID: remoteID, Message: "merge", Author: "tester"})
	if err != nil || session.State != domain.MergeSessionReady || len(session.AutomaticRecords) != 3 || len(session.Conflicts) != 0 {
		t.Fatalf("StartMerge() = %+v, %v", session, err)
	}
	merged, err := svc.CommitMerge(ctx, session.ID)
	if err != nil {
		t.Fatalf("CommitMerge() error = %v", err)
	}
	mergedBytes, err := os.ReadFile(localObjectPath(repo, merged.ID))
	if err != nil {
		t.Fatalf("read merged snapshot: %v", err)
	}
	var mergedObject domain.ContextObject
	if err := json.Unmarshal(mergedBytes, &mergedObject); err != nil {
		t.Fatalf("decode merged snapshot: %v", err)
	}
	if mergedObject.SchemaVersion != 3 || !reflect.DeepEqual(mergedObject.ParentIDs, []string{local.ID, remoteID}) || len(mergedObject.Notes) != 3 {
		t.Fatalf("merged snapshot = %+v", mergedObject)
	}
	var mergedRemote domain.Note
	for _, note := range mergedObject.Notes {
		if note.ID == remoteNote.ID {
			mergedRemote = note
		}
	}
	if !reflect.DeepEqual(mergedRemote.Topics, remoteNote.Topics) {
		t.Fatalf("merged remote note metadata = %+v", mergedRemote)
	}
	status, err := svc.Status(ctx)
	if err != nil || status.ContextCommitID != merged.ID || status.PendingNotes != 0 {
		t.Fatalf("Status() = %+v, %v", status, err)
	}
	history, err := svc.AssembleContext(ctx, domain.ContextQuery{
		Anchor:  domain.ContextAnchor{Kind: domain.AnchorEntity, EntityKey: "example.Run"},
		History: domain.HistoryAll,
	})
	if err != nil || len(history.Items) != 3 {
		t.Fatalf("AssembleContext(merge history) = %+v, %v; want three current notes without duplicated ancestry", history, err)
	}
	for _, item := range history.Items {
		if item.Historical {
			t.Fatalf("merge history duplicated current observation as historical: %+v", history.Items)
		}
	}
	committedSession, err := svc.MergeSession(ctx, session.ID)
	if err != nil || committedSession.State != domain.MergeSessionCommitted {
		t.Fatalf("MergeSession() = %+v, %v", committedSession, err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := os.Remove(localDatabasePath(repo)); err != nil {
		t.Fatalf("remove projection: %v", err)
	}
	svc, err = Open(ctx, repo)
	if err != nil {
		t.Fatalf("reopen service: %v", err)
	}
	rebuilt, err := svc.Rebuild(ctx, merged.ID)
	if err != nil || rebuilt.ContextCommitID != merged.ID || rebuilt.RestoredCommits != 4 {
		t.Fatalf("Rebuild() = %+v, %v", rebuilt, err)
	}
	remotePath := t.TempDir()
	if _, err := svc.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote() error = %v", err)
	}
	pushed, err := svc.PushRemote(ctx, "origin")
	if err != nil || pushed.Outcome != "pushed" || pushed.TransferredObjects != 4 {
		t.Fatalf("PushRemote() = %+v, %v", pushed, err)
	}
	replicaPath := filepath.Join(t.TempDir(), "replica")
	git(t, repo, "clone", "--quiet", repo, replicaPath)
	replica, err := Open(ctx, replicaPath)
	if err != nil {
		t.Fatalf("Open(replica) error = %v", err)
	}
	defer replica.Close()
	if err := replica.Init(ctx); err != nil {
		t.Fatalf("Init(replica) error = %v", err)
	}
	if _, err := replica.Update(ctx); err != nil {
		t.Fatalf("Update(replica) error = %v", err)
	}
	if _, err := replica.AddRemote(ctx, "origin", remotePath); err != nil {
		t.Fatalf("AddRemote(replica) error = %v", err)
	}
	pulled, err := replica.PullRemote(ctx, "origin")
	if err != nil || pulled.Outcome != "pulled" || pulled.TransferredObjects != 4 {
		t.Fatalf("PullRemote() = %+v, %v", pulled, err)
	}
	replicaStatus, err := replica.Status(ctx)
	if err != nil || replicaStatus.ContextCommitID != merged.ID {
		t.Fatalf("Status(replica) = %+v, %v", replicaStatus, err)
	}
	replicaStore, err := replica.openStore(ctx, false)
	if err != nil {
		t.Fatalf("open replica store: %v", err)
	}
	graph, err := replicaStore.ReadObjectChain(merged.ID, replicaStatus.RepositoryID, replicaStatus.RefName)
	if err != nil || len(graph) != 4 {
		t.Fatalf("ReadObjectChain(replica) = %+v, %v", graph, err)
	}
}

func TestCommitMergeUsesAuthoredResolutionForCompetingSuccessors(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	baseNote, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "base", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote(base) error = %v", err)
	}
	base, err := svc.Commit(ctx, CommitInput{Message: "base", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(base) error = %v", err)
	}
	if _, err := svc.ReviseNote(ctx, ReviseNoteInput{NoteID: baseNote.ID, Body: "local successor", Author: "tester"}); err != nil {
		t.Fatalf("ReviseNote(local) error = %v", err)
	}
	local, err := svc.Commit(ctx, CommitInput{Message: "local", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(local) error = %v", err)
	}
	baseBytes, err := os.ReadFile(localObjectPath(repo, base.ID))
	if err != nil {
		t.Fatalf("read base snapshot: %v", err)
	}
	var baseObject domain.ContextObject
	if err := json.Unmarshal(baseBytes, &baseObject); err != nil {
		t.Fatalf("decode base snapshot: %v", err)
	}
	remoteNote := baseObject.Notes[0]
	remoteNote.RevisionID = "remote-successor"
	remoteNote.SupersedesRevisionID = baseObject.Notes[0].RevisionID
	remoteNote.Body = "remote successor"
	remoteNote.Author = "reviewer"
	remoteNote.CreatedAt = time.Now().UTC()
	remoteObject := domain.ContextObject{SchemaVersion: 3, RepositoryID: baseObject.RepositoryID, RefName: baseObject.RefName, ParentIDs: []string{base.ID}, SourceSHA: baseObject.SourceSHA, Message: "remote", Author: "reviewer", CreatedAt: time.Now().UTC(), Provenance: baseObject.Provenance, Entities: baseObject.Entities, Notes: []domain.Note{remoteNote}}
	remoteObject.RevisionMappings = contextRevisionMappings(remoteObject.Notes)
	remoteBytes, err := json.Marshal(remoteObject)
	if err != nil {
		t.Fatalf("marshal remote snapshot: %v", err)
	}
	remoteDigest := blake3.Sum256(remoteBytes)
	remoteID := fmt.Sprintf("%x", remoteDigest[:])
	contextStore, err := svc.openStore(ctx, false)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := contextStore.WriteObject(remoteID, remoteBytes); err != nil {
		t.Fatalf("write remote snapshot: %v", err)
	}
	session, err := svc.StartMerge(ctx, MergeStartInput{LocalSnapshotID: local.ID, RemoteSnapshotID: remoteID, Message: "merge", Author: "tester"})
	if err != nil || session.State != domain.MergeSessionOpen || len(session.Conflicts) != 1 || session.Conflicts[0].NoteID != baseNote.ID {
		t.Fatalf("StartMerge() = %+v, %v", session, err)
	}
	statusBeforeResolve, err := svc.Status(ctx)
	if err != nil || statusBeforeResolve.ContextCommitID != local.ID {
		t.Fatalf("Status() before resolve = %+v, %v", statusBeforeResolve, err)
	}
	if _, err := svc.CommitMerge(ctx, session.ID); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("CommitMerge(unresolved) error = %v, want validation", err)
	}
	if _, err := svc.ResolveMerge(ctx, MergeResolveInput{SessionID: session.ID, ConflictID: session.Conflicts[0].ID, Use: "unexpected"}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ResolveMerge(invalid) error = %v, want validation", err)
	}
	statusAfterRejectedCommit, err := svc.Status(ctx)
	if err != nil || statusAfterRejectedCommit.ContextCommitID != local.ID {
		t.Fatalf("Status() after rejected commit = %+v, %v", statusAfterRejectedCommit, err)
	}
	resolved, err := svc.ResolveMerge(ctx, MergeResolveInput{SessionID: session.ID, ConflictID: session.Conflicts[0].ID, Use: "authored", EntityKey: "example.Run", Kind: "decision", Body: "authored resolution", Author: "merger", Origin: "human"})
	if err != nil || resolved.State != domain.MergeSessionReady || len(resolved.Conflicts) != 1 || resolved.Conflicts[0].Authored == nil {
		t.Fatalf("ResolveMerge() = %+v, %v", resolved, err)
	}
	merged, err := svc.CommitMerge(ctx, session.ID)
	if err != nil {
		t.Fatalf("CommitMerge() error = %v", err)
	}
	mergedBytes, err := os.ReadFile(localObjectPath(repo, merged.ID))
	if err != nil {
		t.Fatalf("read merged snapshot: %v", err)
	}
	var mergedObject domain.ContextObject
	if err := json.Unmarshal(mergedBytes, &mergedObject); err != nil {
		t.Fatalf("decode merged snapshot: %v", err)
	}
	if len(mergedObject.Notes) != 1 || mergedObject.Notes[0].RevisionID != resolved.Conflicts[0].Authored.Note.RevisionID || mergedObject.Notes[0].SupersedesRevisionID != baseObject.Notes[0].RevisionID || mergedObject.Notes[0].Body != "authored resolution" {
		t.Fatalf("merged notes = %+v, want authored successor of base", mergedObject.Notes)
	}
}

func containsBindingState(notes []domain.Note, state domain.NoteBindingState) bool {
	for _, note := range notes {
		if note.BindingState == state {
			return true
		}
	}
	return false
}

func TestLinkedWorktreesUseIndependentLocalStores(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	primary, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open(primary) error = %v", err)
	}
	defer primary.Close()
	if err := primary.Init(context.Background()); err != nil {
		t.Fatalf("Init(primary) error = %v", err)
	}
	if _, err := primary.Update(context.Background()); err != nil {
		t.Fatalf("Update(primary) error = %v", err)
	}
	if _, err := primary.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "primary note", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(primary) error = %v", err)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "-b", "other", linked)
	linkedService, err := Open(context.Background(), linked)
	if err != nil {
		t.Fatalf("Open(linked) error = %v", err)
	}
	defer linkedService.Close()
	if _, err := linkedService.Update(context.Background()); domain.CodeOf(err) != domain.CodeNotInitialized {
		t.Fatalf("Update(linked before init) error = %v, want not_initialized", err)
	}
	if err := linkedService.Init(context.Background()); err != nil {
		t.Fatalf("Init(linked) error = %v", err)
	}
	if _, err := linkedService.Update(context.Background()); err != nil {
		t.Fatalf("Update(linked) error = %v", err)
	}
	linkedStatus, err := linkedService.Status(context.Background())
	if err != nil {
		t.Fatalf("Status(linked) error = %v", err)
	}
	if linkedStatus.PendingNotes != 0 {
		t.Fatalf("linked PendingNotes = %d, want 0", linkedStatus.PendingNotes)
	}
	if _, err := linkedService.AddNote(context.Background(), AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "linked note", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(linked) error = %v", err)
	}
	linkedCommit, err := linkedService.Commit(context.Background(), CommitInput{Message: "linked context", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(linked) error = %v", err)
	}
	if _, err := os.Stat(localObjectPath(linked, linkedCommit.ID)); err != nil {
		t.Fatalf("linked context object missing: %v", err)
	}
	if _, err := os.Stat(localObjectPath(repo, linkedCommit.ID)); !os.IsNotExist(err) {
		t.Fatalf("linked context object exists in primary store: %v", err)
	}
	primaryStatus, err := primary.Status(context.Background())
	if err != nil {
		t.Fatalf("Status(primary) error = %v", err)
	}
	if primaryStatus.PendingNotes != 1 || primaryStatus.ContextCommitID != "" {
		t.Fatalf("primary status = %+v, want isolated pending state", primaryStatus)
	}
}

func TestRebuildRestoresDeletedProjection(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "restore this note", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	first, err := svc.Commit(ctx, CommitInput{Message: "first context", Author: "tester"})
	if err != nil {
		t.Fatalf("first Commit() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "warning", Body: "restore this warning", Author: "tester"}); err != nil {
		t.Fatalf("second AddNote() error = %v", err)
	}
	tip, err := svc.Commit(ctx, CommitInput{Message: "second context", Author: "tester"})
	if err != nil {
		t.Fatalf("second Commit() error = %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	database := localDatabasePath(repo)
	if err := os.Remove(database); err != nil {
		t.Fatalf("Remove(database) error = %v", err)
	}

	rebuilt, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open(rebuild) error = %v", err)
	}
	defer rebuilt.Close()
	result, err := rebuilt.Rebuild(ctx, tip.ID)
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if result.ContextCommitID != tip.ID || result.RestoredCommits != 2 || !result.CoverageComplete {
		t.Fatalf("Rebuild() result = %+v", result)
	}
	log, err := rebuilt.Log(ctx, 10)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if len(log) != 2 || log[0].ID != tip.ID || log[1].ID != first.ID {
		t.Fatalf("Log() = %+v, want restored tip and parent", log)
	}
	contextResult, err := rebuilt.Context(ctx, "example.Run")
	if err != nil {
		t.Fatalf("Context() error = %v", err)
	}
	if len(contextResult.Notes) != 2 || contextResult.Notes[0].Body != "restore this note" || contextResult.Notes[1].Body != "restore this warning" {
		t.Fatalf("Context() notes = %+v", contextResult.Notes)
	}
}

func TestRebuildRejectsV3SnapshotForDifferentSourceBeforeCreatingProjection(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "source-bound context", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	commit, err := svc.Commit(ctx, CommitInput{Message: "source-bound snapshot", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := os.Remove(localDatabasePath(repo)); err != nil {
		t.Fatalf("Remove(index.sqlite) error = %v", err)
	}
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() { println(\"new source\") }\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "advance source")

	rebuilt, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open(rebuild) error = %v", err)
	}
	defer rebuilt.Close()
	if _, err := rebuilt.Rebuild(ctx, commit.ID); domain.CodeOf(err) != domain.CodeStaleWorkingSet {
		t.Fatalf("Rebuild() error = %v, want stale working set", err)
	}
	if _, err := os.Stat(localDatabasePath(repo)); !os.IsNotExist(err) {
		t.Fatalf("index.sqlite exists after rejected source mismatch: %v", err)
	}
}

func TestRebuildRejectsV3SnapshotForDifferentIndexerProvenanceBeforeCreatingProjection(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "provenance-bound context", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	commit, err := svc.Commit(ctx, CommitInput{Message: "provenance-bound snapshot", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := os.Remove(localDatabasePath(repo)); err != nil {
		t.Fatalf("Remove(index.sqlite) error = %v", err)
	}

	rebuilt, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open(rebuild) error = %v", err)
	}
	defer rebuilt.Close()
	rebuilt.indexer = indexCoordinatorFunc(func(_ context.Context, _ string, sourceSHA string) ([]domain.LanguageProjection, error) {
		return []domain.LanguageProjection{{
			Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "changed", SourceSHA: sourceSHA},
			Entities: []domain.Entity{{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: sourceSHA, StructuralHash: "hash"}},
		}}, nil
	})
	if _, err := rebuilt.Rebuild(ctx, commit.ID); domain.CodeOf(err) != domain.CodeStaleWorkingSet {
		t.Fatalf("Rebuild() error = %v, want stale working set", err)
	}
	if _, err := os.Stat(localDatabasePath(repo)); !os.IsNotExist(err) {
		t.Fatalf("index.sqlite exists after rejected provenance mismatch: %v", err)
	}
}

func TestRebuildDoesNotCreateProjectionWhenIndexingFailsOrSourceChanges(t *testing.T) {
	for _, test := range []struct {
		name     string
		indexer  func(string) indexCoordinator
		wantCode domain.ErrorCode
	}{
		{
			name: "indexing fails",
			indexer: func(_ string) indexCoordinator {
				return indexCoordinatorFunc(func(context.Context, string, string) ([]domain.LanguageProjection, error) {
					return nil, errors.New("index failed")
				})
			},
			wantCode: domain.CodeValidation,
		},
		{
			name: "source becomes dirty",
			indexer: func(repo string) indexCoordinator {
				return indexCoordinatorFunc(func(_ context.Context, _ string, sourceSHA string) ([]domain.LanguageProjection, error) {
					writeFile(t, filepath.Join(repo, "dirty.go"), "package example\n")
					return []domain.LanguageProjection{{Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: sourceSHA}}}, nil
				})
			},
			wantCode: domain.CodeRepositoryState,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
			ctx := context.Background()
			svc, err := Open(ctx, repo)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if err := svc.Init(ctx); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if _, err := svc.Update(ctx); err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "restore", Author: "tester"}); err != nil {
				t.Fatalf("AddNote() error = %v", err)
			}
			commit, err := svc.Commit(ctx, CommitInput{Message: "context", Author: "tester"})
			if err != nil {
				t.Fatalf("Commit() error = %v", err)
			}
			if err := svc.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if err := os.Remove(localDatabasePath(repo)); err != nil {
				t.Fatalf("remove projection: %v", err)
			}

			rebuilt, err := Open(ctx, repo)
			if err != nil {
				t.Fatalf("Open(rebuild) error = %v", err)
			}
			defer rebuilt.Close()
			rebuilt.indexer = test.indexer(repo)
			if _, err := rebuilt.Rebuild(ctx, commit.ID); domain.CodeOf(err) != test.wantCode {
				t.Fatalf("Rebuild() error = %v, want %s", err, test.wantCode)
			}
			if _, err := os.Stat(localDatabasePath(repo)); !os.IsNotExist(err) {
				t.Fatalf("projection database exists after pre-publish failure: %v", err)
			}
		})
	}
}

func TestRebuildRejectsMalformedCommitIDBeforeIndexing(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	called := false
	svc.indexer = indexCoordinatorFunc(func(context.Context, string, string) ([]domain.LanguageProjection, error) {
		called = true
		return nil, nil
	})
	if _, err := svc.Rebuild(context.Background(), "not-an-id"); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("Rebuild() error = %v, want validation", err)
	}
	if called {
		t.Fatal("Rebuild() called the indexer for a malformed commit ID")
	}
}

func TestRebuildRejectsMissingObjectBeforeIndexing(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	called := false
	svc.indexer = indexCoordinatorFunc(func(context.Context, string, string) ([]domain.LanguageProjection, error) {
		called = true
		return nil, nil
	})
	if _, err := svc.Rebuild(context.Background(), strings.Repeat("a", 64)); domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("Rebuild() error = %v, want local storage", err)
	}
	if called {
		t.Fatal("Rebuild() called the indexer before validating the selected object ancestry")
	}
}

func TestIndexersReturnsTypedErrorWhenConfigurationLookupFails(t *testing.T) {
	t.Setenv("APPDATA", "")
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(context.Background(), repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if _, err := svc.Indexers(context.Background()); domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("Indexers() error = %v, want local storage", err)
	}
}

func TestRebuildPreservesExistingPendingProjection(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "committed", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(committed) error = %v", err)
	}
	commit, err := svc.Commit(ctx, CommitInput{Message: "context", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "warning", Body: "must remain pending", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(pending) error = %v", err)
	}
	if _, err := svc.Rebuild(ctx, commit.ID); domain.CodeOf(err) != domain.CodeWorkingSetDirty {
		t.Fatalf("Rebuild() error = %v, want working set dirty", err)
	}
	status, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.ContextCommitID != commit.ID || status.PendingNotes != 1 {
		t.Fatalf("status after rejected rebuild = %+v", status)
	}
}

func TestRebuildRejectsDetachedHead(t *testing.T) {
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	ctx := context.Background()
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "context", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	commit, err := svc.Commit(ctx, CommitInput{Message: "context", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	git(t, repo, "checkout", "--detach")
	if _, err := svc.Rebuild(ctx, commit.ID); domain.CodeOf(err) != domain.CodeRepositoryState {
		t.Fatalf("Rebuild() error = %v, want repository state", err)
	}
}

func newContextRemoteServer(t *testing.T) *httptest.Server {
	t.Helper()
	github := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/repos/acme/thread-keep" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.Header.Get("Authorization") {
		case "Bearer writer-token":
			_, _ = writer.Write([]byte(`{"permissions":{"push":true,"pull":true}}`))
		case "Bearer reader-token":
			_, _ = writer.Write([]byte(`{"permissions":{"push":false,"pull":true}}`))
		default:
			writer.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(github.Close)
	store, err := server.OpenStorage(t.TempDir(), "")
	if err != nil {
		t.Fatalf("OpenStorage() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := server.NewHandler(store, server.Config{
		GitHubAPIBaseURL: github.URL,
		Repositories: map[string]server.RepositoryConfig{
			"repo-1": {GitHubOwner: "acme", GitHubRepo: "thread-keep"},
		},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestPushAndPullSharedContextOverHTTPRemote(t *testing.T) {
	ctx := context.Background()
	primaryRepo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	secondaryRepo := filepath.Join(t.TempDir(), "secondary")
	git(t, primaryRepo, "clone", "--no-local", primaryRepo, secondaryRepo)
	server := newContextRemoteServer(t)
	remoteAddress := server.URL + "/v1/repositories/repo-1"
	t.Setenv(remote.TokenEnvironmentVariable, "writer-token")

	primary, err := Open(ctx, primaryRepo)
	if err != nil {
		t.Fatalf("Open(primary) error = %v", err)
	}
	defer primary.Close()
	if err := primary.Init(ctx); err != nil {
		t.Fatalf("Init(primary) error = %v", err)
	}
	if _, err := primary.Update(ctx); err != nil {
		t.Fatalf("Update(primary) error = %v", err)
	}
	if _, err := primary.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "share context over http", Author: "tester"}); err != nil {
		t.Fatalf("AddNote(primary) error = %v", err)
	}
	if _, err := primary.Commit(ctx, CommitInput{Message: "share context", Author: "tester"}); err != nil {
		t.Fatalf("Commit(primary) error = %v", err)
	}
	if _, err := primary.AddRemote(ctx, "origin", remoteAddress); err != nil {
		t.Fatalf("AddRemote(primary) error = %v", err)
	}
	pushed, err := primary.PushRemote(ctx, "origin")
	if err != nil || pushed.Outcome != "pushed" || pushed.TransferredObjects != 1 {
		t.Fatalf("PushRemote() = %+v, %v", pushed, err)
	}
	repeat, err := primary.PushRemote(ctx, "origin")
	if err != nil || repeat.Outcome != "up_to_date" {
		t.Fatalf("PushRemote(repeat) = %+v, %v", repeat, err)
	}

	secondary, err := Open(ctx, secondaryRepo)
	if err != nil {
		t.Fatalf("Open(secondary) error = %v", err)
	}
	defer secondary.Close()
	if err := secondary.Init(ctx); err != nil {
		t.Fatalf("Init(secondary) error = %v", err)
	}
	if _, err := secondary.Update(ctx); err != nil {
		t.Fatalf("Update(secondary) error = %v", err)
	}
	if _, err := secondary.AddRemote(ctx, "origin", remoteAddress); err != nil {
		t.Fatalf("AddRemote(secondary) error = %v", err)
	}
	pulled, err := secondary.PullRemote(ctx, "origin")
	if err != nil || pulled.Outcome != "pulled" || pulled.TransferredObjects != 1 {
		t.Fatalf("PullRemote() = %+v, %v", pulled, err)
	}
	contextResult, err := secondary.Context(ctx, "example.Run")
	if err != nil || len(contextResult.Notes) != 1 || contextResult.Notes[0].Body != "share context over http" {
		t.Fatalf("Context() after pull = %+v, %v", contextResult, err)
	}
}

func TestHTTPRemoteRequiresPushPermissionButAllowsFetch(t *testing.T) {
	ctx := context.Background()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	server := newContextRemoteServer(t)
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if _, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "kept local by read-only token", Author: "tester"}); err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	if _, err := svc.Commit(ctx, CommitInput{Message: "local context", Author: "tester"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if _, err := svc.AddRemote(ctx, "origin", server.URL+"/v1/repositories/repo-1"); err != nil {
		t.Fatalf("AddRemote() error = %v", err)
	}

	t.Setenv(remote.TokenEnvironmentVariable, "reader-token")
	if _, err := svc.PushRemote(ctx, "origin"); domain.CodeOf(err) != domain.CodeAuth {
		t.Fatalf("PushRemote(read-only token) error = %v, want %q", err, domain.CodeAuth)
	}
	fetched, err := svc.FetchRemote(ctx, "origin")
	if err != nil || fetched.Outcome != "empty" {
		t.Fatalf("FetchRemote(read-only token) = %+v, %v, want empty remote", fetched, err)
	}

	t.Setenv(remote.TokenEnvironmentVariable, "")
	if _, err := svc.PushRemote(ctx, "origin"); domain.CodeOf(err) != domain.CodeAuth {
		t.Fatalf("PushRemote(no token) error = %v, want %q", err, domain.CodeAuth)
	}
}

func TestAddRemoteAcceptsHTTPSAddressAndRejectsPlainHTTP(t *testing.T) {
	ctx := context.Background()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\n\nfunc Run() {}\n"})
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer svc.Close()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	added, err := svc.AddRemote(ctx, "origin", "https://context.example.com/base/")
	if err != nil || added.Path != "https://context.example.com/base" {
		t.Fatalf("AddRemote(https) = %+v, %v, want normalized https address", added, err)
	}
	remotes, err := svc.Remotes(ctx)
	if err != nil || len(remotes) != 1 || remotes[0].Path != "https://context.example.com/base" {
		t.Fatalf("Remotes() = %+v, %v, want stored https address", remotes, err)
	}
	if _, err := svc.AddRemote(ctx, "insecure", "http://context.example.com"); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("AddRemote(plain http) error = %v, want %q", err, domain.CodeValidation)
	}
	filesystemRemote := t.TempDir()
	if _, err := svc.AddRemote(ctx, "disk", filesystemRemote); err != nil {
		t.Fatalf("AddRemote(filesystem) error = %v", err)
	}
}

func newGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Thread Keep Test")
	git(t, repo, "config", "user.email", "thread-keep@example.test")
	for name, body := range files {
		writeFile(t, filepath.Join(repo, name), body)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "initial")
	return repo
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func localDatabasePath(repo string) string {
	return filepath.Join(repo, ".thread-keep", "index.sqlite")
}

func localObjectDir(repo string) string {
	return filepath.Join(repo, ".thread-keep", "objects")
}

func localObjectPath(repo, identifier string) string {
	return filepath.Join(localObjectDir(repo), identifier+".json")
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "user.useConfigOnly=true", "-C", repo}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func (f indexCoordinatorFunc) Index(ctx context.Context, root, sourceSHA string) ([]domain.LanguageProjection, error) {
	return f(ctx, root, sourceSHA)
}
