package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tae2089/thread-keep/internal/app"
)

type testClient struct {
	session *mcp.ClientSession
}

func newTestRepo(t *testing.T, initialize bool) string {
	t.Helper()
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Thread Keep Test")
	git("config", "user.email", "thread-keep@example.test")
	if err := os.WriteFile(filepath.Join(repo, "example.go"), []byte("package example\n\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	git("add", "example.go")
	git("commit", "-qm", "initial source")
	if initialize {
		ctx := context.Background()
		svc, err := app.Open(ctx, repo)
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
	}
	return repo
}

func startTestServer(t *testing.T, defaultRepo string) *testClient {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	server := New(defaultRepo)
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("Server.Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "thread-keep-test", Version: "0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("Client.Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return &testClient{session: clientSession}
}

func (c *testClient) callTool(t *testing.T, name string, arguments string) (string, bool) {
	t.Helper()
	var input map[string]any
	if err := json.Unmarshal([]byte(arguments), &input); err != nil {
		t.Fatalf("decode %s test arguments: %v", name, err)
	}
	result, err := c.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: input})
	if err != nil {
		t.Fatalf("CallTool(%s) error = %v", name, err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("tool %s content = %+v, want one text block", name, result.Content)
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("tool %s content = %+v, want one text block", name, result.Content)
	}
	return content.Text, result.IsError
}

func TestListsAllToolsWithStableSchemas(t *testing.T) {
	client := startTestServer(t, newTestRepo(t, true))
	result, err := client.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(result.Tools) != 10 {
		t.Fatalf("ListTools() count = %d, want 10", len(result.Tools))
	}
	encoded, err := json.Marshal(result.Tools)
	if err != nil {
		t.Fatalf("marshal listed tools: %v", err)
	}
	for _, name := range []string{"search", "context_get", "context_for_change", "context_for_entity", "context_query", "related_context", "note_add", "note_revise", "status", "diff"} {
		if !strings.Contains(string(encoded), fmt.Sprintf("%q", name)) {
			t.Fatalf("ListTools() missing %s: %s", name, encoded)
		}
	}
	if strings.Contains(string(encoded), `"origin"`) {
		t.Fatalf("ListTools() exposes an origin input: %s", encoded)
	}
	if strings.Contains(string(encoded), `"expand"`) || strings.Contains(string(encoded), `"applicability"`) {
		t.Fatalf("ListTools() exposes removed relation inputs: %s", encoded)
	}
	if count := strings.Count(string(encoded), `"repo"`); count != len(result.Tools) {
		t.Fatalf("ListTools() repo property count = %d, want %d: %s", count, len(result.Tools), encoded)
	}
}

func TestToolsRouteExplicitRepoAheadOfDefault(t *testing.T) {
	defaultRepo := newTestRepo(t, true)
	explicitRepo := newTestRepo(t, true)
	client := startTestServer(t, defaultRepo)

	arguments := fmt.Sprintf(`{"repo":%q,"entity_key":"example.Run","kind":"decision","body":"explicit repository note"}`, explicitRepo)
	if text, isError := client.callTool(t, "note_add", arguments); isError || !strings.Contains(text, "explicit repository note") {
		t.Fatalf("note_add(explicit repo) = %q (isError=%v)", text, isError)
	}
	if text, isError := client.callTool(t, "status", `{}`); isError || !strings.Contains(text, `"pending_notes":0`) {
		t.Fatalf("status(default repo) = %q (isError=%v), want no pending notes", text, isError)
	}
	arguments = fmt.Sprintf(`{"repo":%q}`, explicitRepo)
	if text, isError := client.callTool(t, "status", arguments); isError || !strings.Contains(text, `"pending_notes":1`) {
		t.Fatalf("status(explicit repo) = %q (isError=%v), want one pending note", text, isError)
	}
}

func TestToolsRequireRepoWithoutDefault(t *testing.T) {
	repo := newTestRepo(t, true)
	client := startTestServer(t, "")

	if text, isError := client.callTool(t, "status", `{}`); !isError || !strings.Contains(text, `"code":"validation"`) {
		t.Fatalf("status(no repo) = %q (isError=%v), want validation", text, isError)
	}
	arguments := fmt.Sprintf(`{"repo":%q}`, repo)
	if text, isError := client.callTool(t, "status", arguments); isError || !strings.Contains(text, `"pending_notes":0`) {
		t.Fatalf("status(explicit repo) = %q (isError=%v), want initialized repository status", text, isError)
	}
}

func TestToolsDoNotFallbackFromInvalidExplicitRepo(t *testing.T) {
	defaultRepo := newTestRepo(t, true)
	client := startTestServer(t, defaultRepo)
	arguments := fmt.Sprintf(`{"repo":%q}`, t.TempDir())

	if text, isError := client.callTool(t, "status", arguments); !isError || !strings.Contains(text, `"code":"repository_state"`) {
		t.Fatalf("status(invalid explicit repo) = %q (isError=%v), want repository_state", text, isError)
	}
}

func TestToolsDraftPendingNotesWithForcedAgentOrigin(t *testing.T) {
	client := startTestServer(t, newTestRepo(t, true))

	text, isError := client.callTool(t, "note_add", `{"entity_key":"example.Run","kind":"constraint","body":"retries must stay idempotent","origin":"human","topics":["retry-contract"]}`)
	if isError || !strings.Contains(text, `"origin":"agent"`) || !strings.Contains(text, `"example.Run"`) || !strings.Contains(text, `"topics":["retry-contract"]`) {
		t.Fatalf("note_add = %q (isError=%v), want pending note with forced agent origin", text, isError)
	}

	if text, isError = client.callTool(t, "status", `{}`); isError || !strings.Contains(text, `"pending_notes":1`) {
		t.Fatalf("status = %q (isError=%v), want one pending note", text, isError)
	}
	if text, isError = client.callTool(t, "diff", `{}`); isError || !strings.Contains(text, "retries must stay idempotent") {
		t.Fatalf("diff = %q (isError=%v)", text, isError)
	}
	if text, isError = client.callTool(t, "search", `{"query":"idempotent"}`); isError || !strings.Contains(text, "example.Run") {
		t.Fatalf("search = %q (isError=%v)", text, isError)
	}
	if text, isError = client.callTool(t, "context_get", `{"entity_key":"example.Run"}`); isError || !strings.Contains(text, "retries must stay idempotent") {
		t.Fatalf("context_get = %q (isError=%v)", text, isError)
	}
	if text, isError = client.callTool(t, "related_context", `{"entity_key":"example.Run","limit":5}`); isError {
		t.Fatalf("related_context = %q (isError=%v)", text, isError)
	}
	if text, isError = client.callTool(t, "context_for_entity", `{"entity_key":"example.Run","kinds":["constraint"]}`); isError || !strings.Contains(text, "retries must stay idempotent") || !strings.Contains(text, `"kind":"direct_entity"`) {
		t.Fatalf("context_for_entity = %q (isError=%v)", text, isError)
	}
	if text, isError = client.callTool(t, "context_query", `{"query":"idempotent","kinds":["constraint"]}`); isError || !strings.Contains(text, "retries must stay idempotent") || !strings.Contains(text, `"kind":"text_note_body"`) {
		t.Fatalf("context_query = %q (isError=%v)", text, isError)
	}
	if text, isError = client.callTool(t, "context_query", `{"query":"retry-contract","topics":["retry-contract"]}`); isError || !strings.Contains(text, "retries must stay idempotent") || !strings.Contains(text, `"kind":"topic_exact"`) {
		t.Fatalf("context_query(topic) = %q (isError=%v)", text, isError)
	}

	if text, isError = client.callTool(t, "note_add", `{"entity_key":"missing.Entity","kind":"intent","body":"nope"}`); !isError || !strings.Contains(text, "entity_not_found") {
		t.Fatalf("note_add(missing entity) = %q (isError=%v), want typed entity_not_found", text, isError)
	}
}

func TestNoteReviseCreatesSuccessorWithAgentOrigin(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t, true)
	client := startTestServer(t, repo)

	text, isError := client.callTool(t, "note_add", `{"entity_key":"example.Run","kind":"intent","body":"first revision","topics":["first-topic"]}`)
	if isError {
		t.Fatalf("note_add = %q", text)
	}
	var note struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &note); err != nil || note.ID == "" {
		t.Fatalf("note id from %q: %v", text, err)
	}

	committer, err := app.Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open(committer) error = %v", err)
	}
	defer committer.Close()
	if _, err := committer.Commit(ctx, app.CommitInput{Message: "commit first revision", Author: "tester"}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if text, isError = client.callTool(t, "context_for_change", `{}`); isError || !strings.Contains(text, `"kind":"change"`) {
		t.Fatalf("context_for_change = %q (isError=%v)", text, isError)
	}

	text, isError = client.callTool(t, "note_revise", fmt.Sprintf(`{"note_id":%q,"body":"second revision","topics":["second-topic"]}`, note.ID))
	if isError || !strings.Contains(text, `"origin":"agent"`) || !strings.Contains(text, "second revision") || !strings.Contains(text, `"supersedes_revision_id"`) || !strings.Contains(text, `"topics":["second-topic"]`) {
		t.Fatalf("note_revise = %q (isError=%v), want agent-origin successor revision", text, isError)
	}
	if text, isError = client.callTool(t, "context_for_entity", `{"entity_key":"example.Run","history":"all"}`); isError || !strings.Contains(text, "first revision") || !strings.Contains(text, "second revision") || !strings.Contains(text, `"historical":true`) {
		t.Fatalf("context_for_entity(history) = %q (isError=%v)", text, isError)
	}
}

func TestToolsReportNotInitializedRepository(t *testing.T) {
	client := startTestServer(t, newTestRepo(t, false))
	if text, isError := client.callTool(t, "status", `{}`); !isError || !strings.Contains(text, "not_initialized") {
		t.Fatalf("status(uninitialized) = %q (isError=%v), want typed not_initialized", text, isError)
	}
}
