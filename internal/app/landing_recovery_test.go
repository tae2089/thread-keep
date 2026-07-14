package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
	"github.com/zeebo/blake3"
)

func TestRecoverAndCommitLandingBuildsManualReceiptAtExactMergeSource(t *testing.T) {
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
		t.Fatalf("Update() error = %v", err)
	}
	state, key, err := svc.mutableKey(ctx)
	if err != nil {
		t.Fatalf("mutableKey() error = %v", err)
	}
	contextStore, err := svc.openStore(ctx, false)
	if err != nil {
		t.Fatalf("openStore() error = %v", err)
	}
	snapshot, err := contextStore.CommitSnapshot(ctx, key)
	if err != nil {
		t.Fatalf("CommitSnapshot() error = %v", err)
	}
	baseSource := strings.Repeat("1", 40)
	canonicalEntities := append([]domain.Entity(nil), snapshot.Entities...)
	for index := range canonicalEntities {
		canonicalEntities[index].SourceSHA = baseSource
	}
	canonical := domain.ContextObject{SchemaVersion: 3, RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: baseSource, Message: "canonical", Author: "tester", CreatedAt: time.Now().UTC(), Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: baseSource}}, Entities: canonicalEntities}
	contents, err := json.Marshal(canonical)
	if err != nil {
		t.Fatalf("Marshal(canonical) error = %v", err)
	}
	digest := blake3.Sum256(contents)
	canonicalID := fmt.Sprintf("%x", digest[:])
	change := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}
	intentID := domain.LandingIntentID(domain.PlanFingerprint{RepositoryID: key.RepositoryID, TargetRef: key.RefName, Change: change, HeadSourceSHA: state.HeadSHA})
	expected := remote.Ref{RefName: key.RefName, CommitID: canonicalID, SourceSHA: baseSource, Version: 1}
	bundle := remote.LandingRecoveryBundle{Intent: domain.LandingIntent{ID: intentID, RepositoryID: key.RepositoryID, TargetRef: key.RefName, Change: change, SourceMergeSHA: state.HeadSHA, State: domain.LandingRecovering}, ExpectedRef: expected}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/landings/"+intentID+"/recovery"):
			_ = json.NewEncoder(writer).Encode(bundle)
		case strings.Contains(request.URL.Path, "/refs/"):
			_ = json.NewEncoder(writer).Encode(expected)
		case strings.HasSuffix(request.URL.Path, "/objects/"+canonicalID):
			writer.Header().Set("Content-Type", "application/octet-stream")
			_, _ = writer.Write(contents)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	if _, err := svc.AddRemote(ctx, "origin", server.URL+"/v1/repositories/repo"); err != nil {
		t.Fatalf("AddRemote() error = %v", err)
	}
	session, err := svc.RecoverLanding(ctx, "origin", intentID)
	if err != nil || session.State != domain.LandingSessionReady || session.SourceSHA != state.HeadSHA || session.ExpectedRemoteCommitID != canonicalID {
		t.Fatalf("RecoverLanding() = %+v, %v", session, err)
	}
	commit, err := svc.CommitLanding(ctx, LandingCommitInput{SessionID: session.ID, Message: "recover context", Author: "tester"})
	if err != nil {
		t.Fatalf("CommitLanding() error = %v", err)
	}
	object, err := contextStore.ReadContextObject(commit.ID, key.RepositoryID, key.RefName)
	if err != nil || object.SchemaVersion != 4 || object.SourceSHA != state.HeadSHA || len(object.LandingReceipts) != 1 || object.LandingReceipts[0].ID != intentID || object.LandingReceipts[0].Resolver != "manual" {
		t.Fatalf("manual landing object = %+v, %v", object, err)
	}
}
