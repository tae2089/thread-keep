package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/tae2089/thread-keep/internal/app"
	"github.com/tae2089/thread-keep/internal/domain"
)

func TestRunnerCLIHappyPathAndJSONErrors(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() {}\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "add Go source")

	runJSONCLI(t, repo, "init")
	runJSONCLI(t, repo, "update")
	runJSONCLI(t, repo, "note", "add", "example.Run", "--kind", "intent", "--body", "run the example")

	status := runJSONCLI(t, repo, "status")
	if !strings.Contains(status, `"pending_notes":1`) {
		t.Fatalf("status output = %s", status)
	}
	search := runJSONCLI(t, repo, "search", "Run")
	if !strings.Contains(search, `"entity_key":"example.Run"`) {
		t.Fatalf("search output = %s", search)
	}
	contextJSON := runJSONCLI(t, repo, "context", "get", "example.Run")
	if !strings.Contains(contextJSON, `"entity_key":"example.Run"`) || !strings.Contains(contextJSON, "run the example") {
		t.Fatalf("context output = %s", contextJSON)
	}
	diff := runJSONCLI(t, repo, "diff")
	if !strings.Contains(diff, `"body":"run the example"`) {
		t.Fatalf("diff output = %s", diff)
	}
	commit := runJSONCLI(t, repo, "commit", "-m", "document run", "--author", "tester")
	if !strings.Contains(commit, `"message":"document run"`) {
		t.Fatalf("commit output = %s", commit)
	}
	log := runJSONCLI(t, repo, "log")
	if !strings.Contains(log, `"message":"document run"`) {
		t.Fatalf("log output = %s", log)
	}
	contextOutput := runCLI(t, repo, "context", "get", "example.Run")
	if !strings.Contains(contextOutput, "example.Run") || !strings.Contains(contextOutput, "run the example") {
		t.Fatalf("context output = %s", contextOutput)
	}

	var stdout, stderr bytes.Buffer
	code := execute(repo, []string{"--json", "note", "add", "missing.Entity", "--kind", "intent", "--body", "missing"}, &stdout, &stderr)
	assertJSONError(t, code, 7, stdout.String(), stderr.String(), "entity_not_found")
	stdout.Reset()
	stderr.Reset()
	code = execute(repo, []string{"--json", "commit"}, &stdout, &stderr)
	assertJSONError(t, code, 2, stdout.String(), stderr.String(), "validation")
}

func TestRunnerUsesParsedJSONFlagForErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := execute(t.TempDir(), []string{"search", "--", "--json"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("positional --json exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), `"version":1`) || !strings.HasPrefix(stderr.String(), "error:") {
		t.Fatalf("positional --json must retain human error output, stderr=%s", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = execute(t.TempDir(), []string{"--json", "status"}, &stdout, &stderr)
	if code != 3 || !strings.Contains(stderr.String(), `"version":1`) {
		t.Fatalf("parsed json flag exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestRunnerUsesJSONErrorsForCommandGroupsWithoutSubcommand(t *testing.T) {
	for _, name := range []string{"context", "indexers", "landing", "note"} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := execute(t.TempDir(), []string{"--json", name}, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), `"code":"validation"`) {
				t.Fatalf("%s exit=%d stdout=%s stderr=%s", name, code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunnerRequiresMergeSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := execute(t.TempDir(), []string{"--json", "context", "merge"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), `"code":"validation"`) {
		t.Fatalf("context merge exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestRunnerListsIndexersWithoutInitialization(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "app.ts"), "export function run() {}\n")
	git(t, repo, "add", "app.ts")
	git(t, repo, "commit", "-m", "add TypeScript source")

	output := runJSONCLI(t, repo, "indexers", "list")
	var statuses []struct {
		Language string `json:"language"`
		PackID   string `json:"pack_id"`
		State    string `json:"state"`
		Detected bool   `json:"detected"`
	}
	if err := json.Unmarshal([]byte(output), &statuses); err != nil {
		t.Fatalf("decode indexers list: %v", err)
	}
	want := []struct {
		Language string `json:"language"`
		PackID   string `json:"pack_id"`
		State    string `json:"state"`
		Detected bool   `json:"detected"`
	}{
		{Language: "go", PackID: "builtin/go", State: "builtin"},
		{Language: "typescript", PackID: "thread-keep-index-typescript", State: "missing", Detected: true},
		{Language: "javascript", PackID: "thread-keep-index-javascript", State: "missing"},
		{Language: "python", PackID: "thread-keep-index-python", State: "missing"},
		{Language: "java", PackID: "thread-keep-index-java", State: "missing"},
		{Language: "kotlin", PackID: "thread-keep-index-kotlin", State: "missing"},
		{Language: "rust", PackID: "thread-keep-index-rust", State: "missing"},
	}
	if len(statuses) != len(want) {
		t.Fatalf("indexers list = %#v, want %#v", statuses, want)
	}
	for index := range want {
		if statuses[index] != want[index] {
			t.Fatalf("indexers list = %#v, want %#v", statuses, want)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "thread-keep")); !os.IsNotExist(err) {
		t.Fatalf("indexers list created context storage: %v", err)
	}
}

func TestRunnerRevisesCommittedNote(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() {}\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "add source")
	runJSONCLI(t, repo, "init")
	runJSONCLI(t, repo, "update")
	added := runJSONCLI(t, repo, "note", "add", "example.Run", "--kind", "intent", "--body", "first")
	var note struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(added), &note); err != nil || note.ID == "" {
		t.Fatalf("note add result = %s, decode error = %v", added, err)
	}
	runJSONCLI(t, repo, "commit", "-m", "first note", "--author", "tester")

	revised := runJSONCLI(t, repo, "note", "revise", note.ID, "--body", "second", "--author", "reviewer")
	var result struct {
		ID                   string `json:"id"`
		RevisionID           string `json:"revision_id"`
		SupersedesRevisionID string `json:"supersedes_revision_id"`
		Pending              bool   `json:"pending"`
	}
	if err := json.Unmarshal([]byte(revised), &result); err != nil || result.ID != note.ID || result.RevisionID == "" || result.SupersedesRevisionID == "" || !result.Pending {
		t.Fatalf("note revise result = %s, decode error = %v", revised, err)
	}
}

func TestRunnerConfirmsNeedsReviewNote(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() {}\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "add source")
	runJSONCLI(t, repo, "init")
	runJSONCLI(t, repo, "update")
	added := runJSONCLI(t, repo, "note", "add", "example.Run", "--kind", "warning", "--body", "review me")
	var note struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(added), &note); err != nil || note.ID == "" {
		t.Fatalf("note add result = %s, decode error = %v", added, err)
	}
	runJSONCLI(t, repo, "commit", "-m", "first note", "--author", "tester")
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() { println(\"changed\") }\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "change source")
	runJSONCLI(t, repo, "update")

	confirmed := runJSONCLI(t, repo, "note", "review", note.ID, "--entity", "example.Run")
	var result struct {
		BindingState string `json:"binding_state"`
		Pending      bool   `json:"pending"`
	}
	if err := json.Unmarshal([]byte(confirmed), &result); err != nil || result.BindingState != "active" || !result.Pending {
		t.Fatalf("note review result = %s, decode error = %v", confirmed, err)
	}
}

func TestRunnerReturnsBoundedRelatedContext(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "worker.go"), "package example\n\ntype Worker struct{}\n\nfunc (w *Worker) Run() {}\n\nfunc Helper() {}\n")
	git(t, repo, "add", "worker.go")
	git(t, repo, "commit", "-m", "add worker")
	runJSONCLI(t, repo, "init")
	runJSONCLI(t, repo, "update")
	output := runJSONCLI(t, repo, "context", "related", "example.Worker.Run", "--limit", "1")
	var related []struct {
		EntityKey string `json:"entity_key"`
		EdgeKind  string `json:"edge_kind"`
		Fresh     bool   `json:"fresh"`
	}
	if err := json.Unmarshal([]byte(output), &related); err != nil || len(related) != 1 || related[0].EntityKey != "example.Worker" || related[0].EdgeKind != "method_owner" || !related[0].Fresh {
		t.Fatalf("context related result = %s, decode error = %v", output, err)
	}
}

func TestRunnerAssemblesEntityAndTextContext(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "example.go"), "package example\n\nfunc Run() {}\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "add Go source")
	runJSONCLI(t, repo, "init")
	runJSONCLI(t, repo, "update")
	runJSONCLI(t, repo, "note", "add", "example.Run", "--kind", "constraint", "--body", "run context assembly safely")

	entity := runJSONCLI(t, repo, "context", "for-entity", "example.Run", "--kind", "constraint")
	if !strings.Contains(entity, `"anchor":{"kind":"entity"`) || !strings.Contains(entity, "run context assembly safely") || !strings.Contains(entity, `"kind":"direct_entity"`) {
		t.Fatalf("context for-entity = %s", entity)
	}
	text := runJSONCLI(t, repo, "context", "query", "context assembly")
	if !strings.Contains(text, `"anchor":{"kind":"text"`) || !strings.Contains(text, "run context assembly safely") || !strings.Contains(text, `"kind":"text_note_body"`) {
		t.Fatalf("context query = %s", text)
	}
}

func TestRunnerAssemblesContextForChange(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "example.go"), "package example\n\nfunc Run() {}\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "add Go source")
	runJSONCLI(t, repo, "init")
	runJSONCLI(t, repo, "update")
	runJSONCLI(t, repo, "note", "add", "example.Run", "--kind", "constraint", "--body", "preserve run behavior")
	runJSONCLI(t, repo, "commit", "-m", "base context", "--author", "tester")
	writeFile(t, filepath.Join(repo, "example.go"), "package example\n\nfunc Run() { println(\"changed\") }\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "change Run")
	runJSONCLI(t, repo, "update")

	result := runJSONCLI(t, repo, "context", "for-change", "--state", "needs_review", "--kind", "constraint")
	if !strings.Contains(result, `"anchor":{"kind":"change"`) || !strings.Contains(result, `"kind":"modified"`) || !strings.Contains(result, "preserve run behavior") {
		t.Fatalf("context for-change = %s", result)
	}
}

func TestRunnerAssemblesHistoricalContext(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "example.go"), "package example\n\nfunc Run() {}\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "add Go source")
	runJSONCLI(t, repo, "init")
	runJSONCLI(t, repo, "update")
	added := runJSONCLI(t, repo, "note", "add", "example.Run", "--kind", "decision", "--body", "first CLI decision")
	var note struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(added), &note); err != nil || note.ID == "" {
		t.Fatalf("decode note add = %s, %v", added, err)
	}
	runJSONCLI(t, repo, "commit", "-m", "first context", "--author", "tester")
	runJSONCLI(t, repo, "note", "revise", note.ID, "--body", "current CLI decision", "--author", "tester")

	history := runJSONCLI(t, repo, "context", "for-entity", "example.Run", "--history", "all")
	if !strings.Contains(history, "first CLI decision") || !strings.Contains(history, "current CLI decision") || !strings.Contains(history, `"historical":true`) {
		t.Fatalf("historical context = %s", history)
	}
}

func TestRunnerAuthorsAndQueriesNoteTopics(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "worker.go"), "package example\n\ntype Worker struct{}\n\nfunc (w *Worker) Run() {}\n")
	git(t, repo, "add", "worker.go")
	git(t, repo, "commit", "-m", "add Worker")
	runJSONCLI(t, repo, "init")
	runJSONCLI(t, repo, "update")
	note := runJSONCLI(t, repo, "note", "add", "example.Worker", "--kind", "constraint", "--body", "evict safely", "--topic", "cache-invalidation")
	if !strings.Contains(note, `"topics":["cache-invalidation"]`) {
		t.Fatalf("note add metadata = %s", note)
	}
	result := runJSONCLI(t, repo, "context", "query", "cache-invalidation", "--topic", "cache-invalidation")
	if !strings.Contains(result, "evict safely") || !strings.Contains(result, `"kind":"topic_exact"`) {
		t.Fatalf("context query topic = %s", result)
	}
}

func TestRunnerConfiguresAndListsFilesystemRemote(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() {}\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "add source")
	runJSONCLI(t, repo, "init")
	remotePath := t.TempDir()
	resolvedRemotePath, err := filepath.EvalSymlinks(remotePath)
	if err != nil {
		t.Fatalf("resolve remote path: %v", err)
	}
	added := runJSONCLI(t, repo, "remote", "add", "origin", remotePath)
	var remoteConfig struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(added), &remoteConfig); err != nil || remoteConfig.Name != "origin" || remoteConfig.Path != resolvedRemotePath {
		t.Fatalf("remote add result = %s, decode error = %v", added, err)
	}
	listed := runJSONCLI(t, repo, "remote", "list")
	var remotes []struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(listed), &remotes); err != nil || len(remotes) != 1 || remotes[0] != remoteConfig {
		t.Fatalf("remote list result = %s, decode error = %v", listed, err)
	}
}

func TestRunnerImportsListsAndShowsCandidatesWithoutCanonicalContext(t *testing.T) {
	repo := newGitRepo(t)
	runJSONCLI(t, repo, "init")
	path := filepath.Join(t.TempDir(), "candidate.json")
	payload := `{"schema_version":1,"provider":"github","repository":"owner/repository","number":42,"state":"open","base_sha":"` + strings.Repeat("a", 40) + `","head_sha":"` + strings.Repeat("b", 40) + `","merge_sha":"","updated_at":"2026-07-12T00:00:00Z","notes":[{"id":"note-1","entity_key":"example.Run","structural_hash":"` + strings.Repeat("c", 64) + `","kind":"intent","body":"candidate draft","author":"reviewer","origin":"provider","created_at":"2026-07-12T00:00:00Z"}]}`
	writeFile(t, path, payload)
	imported := runJSONCLI(t, repo, "candidate", "import", path)
	if !strings.Contains(imported, `"imported":true`) || !strings.Contains(imported, `"draft_notes":1`) {
		t.Fatalf("candidate import = %s", imported)
	}
	listed := runJSONCLI(t, repo, "candidate", "list")
	if !strings.Contains(listed, `"id":"github:owner/repository#42"`) || strings.Contains(listed, "candidate draft") {
		t.Fatalf("candidate list = %s", listed)
	}
	shown := runJSONCLI(t, repo, "candidate", "show", "github:owner/repository#42")
	if !strings.Contains(shown, "candidate draft") || !strings.Contains(shown, `"state":"draft"`) {
		t.Fatalf("candidate show = %s", shown)
	}
	status := runJSONCLI(t, repo, "status")
	if !strings.Contains(status, `"pending_notes":0`) || !strings.Contains(status, `"context_commit_id":""`) {
		t.Fatalf("status after candidate import = %s", status)
	}
}

func TestRunnerRebuildRestoresDeletedProjection(t *testing.T) {
	repo := newGitRepo(t)
	writeFile(t, filepath.Join(repo, "example.go"), "package example\nfunc Run() {}\n")
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "add Go source")
	runJSONCLI(t, repo, "init")
	runJSONCLI(t, repo, "update")
	runJSONCLI(t, repo, "note", "add", "example.Run", "--kind", "intent", "--body", "restore this")
	commitJSON := runJSONCLI(t, repo, "commit", "-m", "restorable context", "--author", "tester")
	var commit struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(commitJSON), &commit); err != nil {
		t.Fatalf("decode commit JSON: %v", err)
	}
	if err := os.Remove(filepath.Join(repo, ".git", "thread-keep", "index.sqlite")); err != nil {
		t.Fatalf("remove projection: %v", err)
	}

	rebuildJSON := runJSONCLI(t, repo, "rebuild", commit.ID)
	if !strings.Contains(rebuildJSON, `"context_commit_id":"`+commit.ID+`"`) || !strings.Contains(rebuildJSON, `"restored_commits":1`) {
		t.Fatalf("rebuild output = %s", rebuildJSON)
	}
	contextJSON := runJSONCLI(t, repo, "context", "get", "example.Run")
	if !strings.Contains(contextJSON, "restore this") {
		t.Fatalf("context output after rebuild = %s", contextJSON)
	}
}

func TestWriteResultRendersHumanRebuildSummary(t *testing.T) {
	var output bytes.Buffer
	result := app.RebuildResult{ContextCommitID: "commit", RestoredCommits: 2, IndexedEntities: 3, SourceSHA: "source", CoverageComplete: true}
	if err := writeResult(&output, false, result); err != nil {
		t.Fatalf("writeResult() error = %v", err)
	}
	for _, expected := range []string{"restored 2 context commits at commit", "indexed 3 entities at source", "coverage complete: true"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("human rebuild output = %q, missing %q", output.String(), expected)
		}
	}
}

func TestWriteResultShowsPendingBindingState(t *testing.T) {
	var output bytes.Buffer
	if err := writeResult(&output, false, []domain.Note{{EntityKey: "example.Run", Kind: domain.NoteWarning, Body: "review this", BindingState: domain.NoteBindingNeedsReview}}); err != nil {
		t.Fatalf("writeResult() error = %v", err)
	}
	if !strings.Contains(output.String(), "needs_review") {
		t.Fatalf("human pending note output = %q, want binding state", output.String())
	}
}

func TestContextAssemblyCommandsDoNotExposeRelationExpansion(t *testing.T) {
	root := newTestRoot(NewRunner(app.Open))
	for _, path := range [][]string{{"context", "for-entity"}, {"context", "for-change"}, {"context", "query"}} {
		command, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("Find(%v) error = %v", path, err)
		}
		if command.Flags().Lookup("expand") != nil {
			t.Fatalf("%v exposes removed --expand flag", path)
		}
	}
}

func TestWriteResultShowsSearchEvidence(t *testing.T) {
	var output bytes.Buffer
	hit := domain.SearchHit{EntityKey: "example.Run", Path: "example.go", Snippet: "run", MatchedFields: []domain.SearchMatchField{domain.SearchMatchName, domain.SearchMatchNoteBody}, MatchedTerms: []string{"run"}, NoteIDs: []string{"note"}, BindingState: domain.NoteBindingActive, Fresh: true}
	if err := writeResult(&output, false, []domain.SearchHit{hit}); err != nil {
		t.Fatalf("writeResult() error = %v", err)
	}
	for _, expected := range []string{"name,note_body", "run", "note", "fresh:true"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("human search output = %q, missing %q", output.String(), expected)
		}
	}
}

func TestWriteResultRendersRemoteSyncEvidence(t *testing.T) {
	var output bytes.Buffer
	result := domain.RemoteSyncResult{RemoteName: "origin", RefName: "refs/contexts/main", LocalTip: "local", RemoteTip: "remote", Outcome: "pushed", TransferredObjects: 2}
	if err := writeResult(&output, false, result); err != nil {
		t.Fatalf("writeResult() error = %v", err)
	}
	for _, expected := range []string{"origin", "refs/contexts/main", "pushed", "local:local", "remote:remote", "objects:2"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("remote human output = %q, missing %q", output.String(), expected)
		}
	}
}

func TestExitCodeMapsAuthErrorsToDedicatedCode(t *testing.T) {
	if got := exitCode(domain.NewError(domain.CodeAuth, os.ErrPermission)); got != 8 {
		t.Fatalf("exitCode(auth) = %d, want 8", got)
	}
}

func execute(repo string, arguments []string, stdout, stderr *bytes.Buffer) int {
	runner := NewRunner(app.Open)
	root := newTestRoot(runner)
	return runner.Execute(context.Background(), root, append([]string{"--repo", repo}, arguments...), stdout, stderr)
}

func runCLI(t *testing.T, repo string, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := execute(repo, args, &stdout, &stderr); code != 0 {
		t.Fatalf("thread-keep %s exit=%d stdout=%s stderr=%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func runJSONCLI(t *testing.T, repo string, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	arguments := append([]string{"--json"}, args...)
	if code := execute(repo, arguments, &stdout, &stderr); code != 0 {
		t.Fatalf("thread-keep --json %s exit=%d stdout=%s stderr=%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("thread-keep --json %s wrote stderr: %s", strings.Join(args, " "), stderr.String())
	}
	var envelope struct {
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("thread-keep --json %s output is not JSON: %v\n%s", strings.Join(args, " "), err, stdout.String())
	}
	if envelope.Version != OutputVersion || len(envelope.Data) == 0 {
		t.Fatalf("thread-keep --json %s envelope = %#v", strings.Join(args, " "), envelope)
	}
	return string(envelope.Data)
}

func assertJSONError(t *testing.T, gotCode, wantCode int, stdout, stderr, wantErrorCode string) {
	t.Helper()
	if gotCode != wantCode || stdout != "" {
		t.Fatalf("error exit=%d stdout=%s stderr=%s, want exit=%d and empty stdout", gotCode, stdout, stderr, wantCode)
	}
	var envelope struct {
		Version int `json:"version"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("error output is not JSON: %v\n%s", err, stderr)
	}
	if envelope.Version != OutputVersion || envelope.Error.Code != wantErrorCode {
		t.Fatalf("error envelope = %#v, want code %q", envelope, wantErrorCode)
	}
}

func newTestRoot(runner *Runner) *cobra.Command {
	root := &cobra.Command{Use: "thread-keep", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("repo", "", "repository")
	root.PersistentFlags().Bool("json", false, "json")
	root.AddCommand(Commands(runner)...)
	return root
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Thread Keep CLI Test")
	git(t, repo, "config", "user.email", "thread-keep@example.test")
	writeFile(t, filepath.Join(repo, "README.md"), "test\n")
	git(t, repo, "add", "README.md")
	git(t, repo, "commit", "-m", "initial")
	return repo
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
