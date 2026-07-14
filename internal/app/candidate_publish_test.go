package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
)

func TestPublishCandidateBuildsRevisionDeltaWithoutChangingLocalRef(t *testing.T) {
	ctx := context.Background()
	repo := newGitRepo(t, map[string]string{"example.go": "package example\nfunc Run() {}\n"})
	svc, err := Open(ctx, repo)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(base) error = %v", err)
	}
	note, err := svc.AddNote(ctx, AddNoteInput{EntityKey: "example.Run", Kind: "intent", Body: "base context", Author: "tester"})
	if err != nil {
		t.Fatalf("AddNote() error = %v", err)
	}
	baseCommit, err := svc.Commit(ctx, CommitInput{Message: "base context", Author: "tester"})
	if err != nil {
		t.Fatalf("Commit(base) error = %v", err)
	}
	baseStatus, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status(base) error = %v", err)
	}
	if err := os.WriteFile(repo+"/example.go", []byte("package example\nfunc Run(input string) {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(source) error = %v", err)
	}
	git(t, repo, "add", "example.go")
	git(t, repo, "commit", "-m", "change source")
	if _, err := svc.Update(ctx); err != nil {
		t.Fatalf("Update(head) error = %v", err)
	}
	if _, err := svc.ReviseNote(ctx, ReviseNoteInput{NoteID: note.ID, Body: "PR revision", Author: "tester"}); err != nil {
		t.Fatalf("ReviseNote() error = %v", err)
	}
	if _, err := svc.Commit(ctx, CommitInput{Message: "PR context", Author: "tester"}); err != nil {
		t.Fatalf("Commit(head) error = %v", err)
	}
	headStatus, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status(head) error = %v", err)
	}
	change := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}
	metadata := remote.CandidatePublicationMetadata{Change: change, BaseSourceSHA: baseStatus.SourceSHA, HeadSourceSHA: headStatus.SourceSHA, BaseContextCommitID: baseCommit.ID}
	var published remote.CandidatePublicationRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			_ = json.NewEncoder(writer).Encode(metadata)
		case http.MethodPut:
			if err := json.NewDecoder(request.Body).Decode(&published); err != nil {
				t.Errorf("decode publish request: %v", err)
			}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(remote.CandidatePublicationResult{Digest: published.Digest, Published: true})
		}
	}))
	t.Cleanup(server.Close)
	if _, err := svc.AddRemote(ctx, "coordinator", server.URL+"/v1/repositories/repo"); err != nil {
		t.Fatalf("AddRemote() error = %v", err)
	}

	result, err := svc.PublishCandidate(ctx, "coordinator", "github:owner/repository#42")
	if err != nil {
		t.Fatalf("PublishCandidate() error = %v", err)
	}
	if !result.Published || result.Digest == "" || len(published.Delta.Records) != 1 || published.Delta.Records[0].Operation != domain.CandidateRevise || published.Delta.Records[0].BaseRevisionID != note.RevisionID {
		t.Fatalf("PublishCandidate() result=%+v request=%+v", result, published)
	}
	after, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status(after publish) error = %v", err)
	}
	if after.ContextCommitID != headStatus.ContextCommitID || after.PendingNotes != 0 || !strings.EqualFold(result.Digest, published.Digest) {
		t.Fatalf("candidate publication mutated local context: before=%+v after=%+v", headStatus, after)
	}
}

func TestBuildCandidateContextDeltaClassifiesAddRebindAndInvalidMutation(t *testing.T) {
	createdAt := time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC)
	baseSource := strings.Repeat("1", 40)
	headSource := strings.Repeat("2", 40)
	baseContextID := strings.Repeat("3", 64)
	change := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}
	metadata := remote.CandidatePublicationMetadata{Change: change, BaseSourceSHA: baseSource, HeadSourceSHA: headSource, BaseContextCommitID: baseContextID}
	baseEntity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Path: "example.go", SourceSHA: baseSource, StructuralHash: strings.Repeat("4", 64)}
	baseNote := domain.Note{ID: "base-note", RevisionID: "base-revision", EntityKey: baseEntity.Key, Kind: domain.NoteIntent, Body: "base", Author: "tester", Origin: "human", CreatedAt: createdAt, BindingState: domain.NoteBindingActive, BindingSourceSHA: baseSource}
	base := domain.ContextObject{SchemaVersion: 3, RepositoryID: "repo", RefName: "refs/contexts/main", SourceSHA: baseSource, Entities: []domain.Entity{baseEntity}, Notes: []domain.Note{baseNote}}

	t.Run("add and rebind", func(t *testing.T) {
		movedEntity := baseEntity
		movedEntity.Key = "moved.Run"
		movedEntity.SourceSHA = headSource
		addedEntity := movedEntity
		addedEntity.Key = "added.Run"
		moved := baseNote
		moved.EntityKey = movedEntity.Key
		moved.BindingSourceSHA = headSource
		added := domain.Note{ID: "added-note", RevisionID: "added-revision", EntityKey: addedEntity.Key, Kind: domain.NoteWarning, Body: "added", Author: "tester", Origin: "human", CreatedAt: createdAt.Add(time.Second), BindingState: domain.NoteBindingActive, BindingSourceSHA: headSource}
		branch := domain.ContextObject{SchemaVersion: 3, RepositoryID: base.RepositoryID, RefName: base.RefName, SourceSHA: headSource, Entities: []domain.Entity{movedEntity, addedEntity}, Notes: []domain.Note{moved, added}}

		delta, err := buildCandidateContextDelta(change, metadata, base, branch)
		if err != nil {
			t.Fatalf("buildCandidateContextDelta() error = %v", err)
		}
		operations := map[domain.CandidateContextOperation]bool{}
		for _, record := range delta.Records {
			operations[record.Operation] = true
		}
		if len(delta.Records) != 2 || !operations[domain.CandidateAdd] || !operations[domain.CandidateRebind] {
			t.Fatalf("candidate records = %+v, want add and rebind", delta.Records)
		}
	})

	t.Run("immutable mutation", func(t *testing.T) {
		entity := baseEntity
		entity.SourceSHA = headSource
		mutated := baseNote
		mutated.Body = "changed without revision"
		mutated.BindingSourceSHA = headSource
		branch := domain.ContextObject{SchemaVersion: 3, RepositoryID: base.RepositoryID, RefName: base.RefName, SourceSHA: headSource, Entities: []domain.Entity{entity}, Notes: []domain.Note{mutated}}
		if _, err := buildCandidateContextDelta(change, metadata, base, branch); domain.CodeOf(err) != domain.CodeValidation {
			t.Fatalf("buildCandidateContextDelta() error = %v, want validation", err)
		}
	})
}
