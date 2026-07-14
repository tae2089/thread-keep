package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/forge"
	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/tae2089/thread-keep/internal/remote"
	"github.com/zeebo/blake3"
)

type failingRunner struct {
	err error
}

func TestCoordinatorAutomaticallyLandsMergedChangeExactlyOnce(t *testing.T) {
	coordinator, store, fakeForge, originalRef := newTestCoordinator(t)
	if _, err := coordinator.IntakeWebhook(context.Background(), http.Header{"X-Github-Delivery": {"delivery-open"}}, []byte(`{"event":"open"}`)); err != nil {
		t.Fatalf("IntakeWebhook(open) error = %v", err)
	}
	delta := domain.CandidateContextDelta{SchemaVersion: 2, Change: fakeForge.change.Key, BaseSourceSHA: fakeForge.change.BaseSHA, HeadSourceSHA: fakeForge.change.HeadSHA, BaseContextCommitID: originalRef.CommitID}
	delta, _ = domain.NormalizeCandidateContextDelta(delta)
	digest, _ := domain.CandidateContextDigest(delta)
	if _, err := coordinator.PublishCandidate(context.Background(), "repo", remote.CandidatePublicationRequest{Delta: delta, Digest: digest}); err != nil {
		t.Fatalf("PublishCandidate() error = %v", err)
	}
	mergeSHA := strings.Repeat("9", 40)
	fakeForge.mu.Lock()
	fakeForge.change.State = forge.ChangeMerged
	fakeForge.change.Merged = true
	fakeForge.change.MergeSHA = mergeSHA
	fakeForge.mu.Unlock()
	result, err := coordinator.IntakeWebhook(context.Background(), http.Header{"X-Github-Delivery": {"delivery-merged"}}, []byte(`{"event":"merged"}`))
	if err != nil || !result.Accepted {
		t.Fatalf("IntakeWebhook(merged) = %+v, %v", result, err)
	}
	for attempt := 0; attempt < 5; attempt++ {
		processed, err := coordinator.RunOne(context.Background(), "landing-worker", time.Now().UTC().Add(time.Duration(attempt)*time.Second))
		if err != nil {
			t.Fatalf("RunOne(%d) error = %v", attempt, err)
		}
		if !processed {
			break
		}
	}
	intentID := domain.LandingIntentID(domain.PlanFingerprint{RepositoryID: "context-repo", TargetRef: "refs/contexts/main", Change: fakeForge.change.Key, HeadSourceSHA: mergeSHA, CandidateDigest: digest})
	intent, err := coordinator.refs.Landing(context.Background(), intentID)
	if err != nil || intent.State != domain.LandingLanded || intent.LandedContextCommitID == "" || intent.FinalPlanID == "" {
		t.Fatalf("Landing() = %+v, %v", intent, err)
	}
	landedRef, err := store.ReadRef(context.Background(), "context-repo", "refs/contexts/main")
	if err != nil || landedRef.Version != originalRef.Version+1 || landedRef.SourceSHA != mergeSHA || landedRef.CommitID != intent.LandedContextCommitID {
		t.Fatalf("landed ref = %+v, %v", landedRef, err)
	}
	contents, err := store.ReadObject(context.Background(), "context-repo", landedRef.CommitID)
	if err != nil {
		t.Fatalf("ReadObject(landed) error = %v", err)
	}
	var object domain.ContextObject
	if err := json.Unmarshal(contents, &object); err != nil || object.SchemaVersion != 4 || len(object.LandingReceipts) != 1 || object.LandingReceipts[0].ID != intentID || object.LandingReceipts[0].CandidateDigest != digest {
		t.Fatalf("landed object = %+v, decode error %v", object, err)
	}
	if _, err := coordinator.IntakeWebhook(context.Background(), http.Header{"X-Github-Delivery": {"delivery-merged-duplicate"}}, []byte(`{"event":"merged-again"}`)); err != nil {
		t.Fatalf("IntakeWebhook(merged duplicate) error = %v", err)
	}
	var finalJobsBeforeDuplicate int64
	if err := coordinator.refs.db.Model(&coordinatorJobRecord{}).Where("kind = ?", finalJobKind).Count(&finalJobsBeforeDuplicate).Error; err != nil {
		t.Fatalf("count final jobs before duplicate error = %v", err)
	}
	logicalKey := DesiredCheckLogicalKey(fakeForge.change.Key, fakeForge.change.HeadSHA)
	desiredBeforeDuplicate, err := coordinator.refs.DesiredCheck(context.Background(), logicalKey)
	if err != nil || desiredBeforeDuplicate.State != forge.CheckReady {
		t.Fatalf("DesiredCheck(before merged duplicate) = %+v, %v", desiredBeforeDuplicate, err)
	}
	if processed, err := coordinator.RunOne(context.Background(), "landing-duplicate-worker", time.Now().UTC()); err != nil || !processed {
		t.Fatalf("RunOne(merged duplicate webhook) = %t, %v", processed, err)
	}
	refAfterDuplicate, _ := store.ReadRef(context.Background(), "context-repo", "refs/contexts/main")
	if refAfterDuplicate != landedRef {
		t.Fatalf("duplicate merge advanced ref: before=%+v after=%+v", landedRef, refAfterDuplicate)
	}
	desiredAfterDuplicate, err := coordinator.refs.DesiredCheck(context.Background(), logicalKey)
	if err != nil || desiredAfterDuplicate != desiredBeforeDuplicate {
		t.Fatalf("DesiredCheck(after merged duplicate) = %+v, %v, want unchanged %+v", desiredAfterDuplicate, err, desiredBeforeDuplicate)
	}
	var finalJobsAfterDuplicate int64
	if err := coordinator.refs.db.Model(&coordinatorJobRecord{}).Where("kind = ?", finalJobKind).Count(&finalJobsAfterDuplicate).Error; err != nil || finalJobsAfterDuplicate != finalJobsBeforeDuplicate {
		t.Fatalf("final jobs after merged duplicate = %d, %v, want unchanged %d", finalJobsAfterDuplicate, err, finalJobsBeforeDuplicate)
	}
}

func TestLandingRefServiceRollsBackReceiptAndIntentOnCASLoss(t *testing.T) {
	store := openTestCoordinatorStore(t)
	refName := "refs/contexts/main"
	current := remote.Ref{RefName: refName, CommitID: strings.Repeat("a", 64), SourceSHA: strings.Repeat("1", 40), Version: 1}
	if _, err := store.CompareAndSwapRef(context.Background(), "repo-id", refName, remote.Ref{RefName: refName}, current); err != nil {
		t.Fatalf("seed ref error = %v", err)
	}
	intent := domain.LandingIntent{ID: strings.Repeat("b", 64), RepositoryID: "repo-id", TargetRef: refName, Change: domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}, SourceMergeSHA: strings.Repeat("2", 40), FinalPlanID: strings.Repeat("c", 64), State: domain.LandingPending}
	if _, err := store.CreateLandingIntent(context.Background(), intent); err != nil {
		t.Fatalf("CreateLandingIntent() error = %v", err)
	}
	if _, err := store.TransitionLanding(context.Background(), intent.ID, domain.LandingPending, domain.LandingRunning); err != nil {
		t.Fatalf("TransitionLanding() error = %v", err)
	}
	object := domain.ContextObject{SchemaVersion: 4, RepositoryID: "repo-id", RefName: refName, ParentIDs: []string{current.CommitID}, SourceSHA: intent.SourceMergeSHA, Message: "land", Author: "thread-keep", CreatedAt: time.Now().UTC(), Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: intent.SourceMergeSHA}}, LandingReceipts: []domain.LandingReceipt{{ID: intent.ID, Provider: "github", ForgeRepository: intent.Change.Repository, ChangeNumber: intent.Change.Number, ContextRepositoryID: "repo-id", TargetRef: refName, FinalPlanID: intent.FinalPlanID, SourceMergeSHA: intent.SourceMergeSHA, BaseContextCommitID: current.CommitID, Resolver: "automatic"}}}
	contents, _ := json.Marshal(object)
	digest := blake3.Sum256(contents)
	next := remote.Ref{RefName: refName, CommitID: formatDigest(digest[:]), SourceSHA: intent.SourceMergeSHA, Version: 2}
	stale := current
	stale.Version = 0
	service := NewLandingRefService(store)
	if _, err := service.Advance(context.Background(), RefAdvanceInput{Expected: stale, Next: next, Object: object}); domain.CodeOf(err) != domain.CodeRemoteConflict {
		t.Fatalf("Advance(stale) error = %v, want remote conflict", err)
	}
	got, _ := store.ReadRef(context.Background(), "repo-id", refName)
	if got != current {
		t.Fatalf("ref changed after rollback: %+v", got)
	}
	landing, _ := store.Landing(context.Background(), intent.ID)
	if landing.State != domain.LandingRunning || landing.LandedContextCommitID != "" {
		t.Fatalf("intent changed after rollback: %+v", landing)
	}
	var receiptCount int64
	if err := store.db.Model(&landingReceiptRecord{}).Where("receipt_id = ?", intent.ID).Count(&receiptCount).Error; err != nil || receiptCount != 0 {
		t.Fatalf("receipt count after rollback = %d, %v", receiptCount, err)
	}
}

func TestCoordinatorBoundsTransientLandingRetriesAndLeavesBlockedIntent(t *testing.T) {
	coordinator, _, fakeForge, _ := newTestCoordinator(t)
	mergeSHA := strings.Repeat("8", 40)
	fakeForge.mu.Lock()
	fakeForge.change.State = forge.ChangeMerged
	fakeForge.change.Merged = true
	fakeForge.change.MergeSHA = mergeSHA
	change := fakeForge.change.Key
	fakeForge.mu.Unlock()
	coordinator.runner = failingRunner{err: domain.NewError(domain.CodeLocalStorage, context.DeadlineExceeded)}
	if _, err := coordinator.IntakeWebhook(context.Background(), http.Header{"X-Github-Delivery": {"delivery-retry"}}, []byte(`{"event":"merged"}`)); err != nil {
		t.Fatalf("IntakeWebhook() error = %v", err)
	}
	if processed, err := coordinator.RunOne(context.Background(), "retry-control", time.Now().UTC()); err != nil || !processed {
		t.Fatalf("RunOne(process webhook) = %t, %v", processed, err)
	}
	if processed, err := coordinator.RunOne(context.Background(), "retry-check", time.Now().UTC().Add(time.Second)); err != nil || !processed {
		t.Fatalf("RunOne(planning check) = %t, %v", processed, err)
	}
	base := time.Now().UTC().Add(time.Second)
	for attempt := 0; attempt < 3; attempt++ {
		_, err := coordinator.RunOne(context.Background(), "retry-worker", base.Add(time.Duration(attempt)*time.Minute))
		if attempt < 2 && domain.CodeOf(err) != domain.CodeLocalStorage {
			t.Fatalf("RunOne(%d) error = %v, want transient local storage", attempt, err)
		}
		if attempt == 2 && err != nil {
			t.Fatalf("RunOne(final retry) error = %v, want blocked terminal", err)
		}
	}
	intentID := domain.LandingIntentID(domain.PlanFingerprint{RepositoryID: "context-repo", TargetRef: "refs/contexts/main", Change: change, HeadSourceSHA: mergeSHA})
	intent, err := coordinator.refs.Landing(context.Background(), intentID)
	if err != nil || intent.State != domain.LandingBlocked || intent.AttemptCount != 3 || intent.LastErrorCode != domain.CodeLocalStorage {
		t.Fatalf("Landing(after retries) = %+v, %v", intent, err)
	}
}

func TestCoordinatorRefCASCompletesManualRecoveryTransaction(t *testing.T) {
	coordinator, storage, fakeForge, current := newTestCoordinator(t)
	mergeSHA := strings.Repeat("7", 40)
	intent := domain.LandingIntent{ID: domain.LandingIntentID(domain.PlanFingerprint{RepositoryID: "context-repo", TargetRef: current.RefName, Change: fakeForge.change.Key, HeadSourceSHA: mergeSHA}), RepositoryID: "context-repo", TargetRef: current.RefName, Change: fakeForge.change.Key, SourceMergeSHA: mergeSHA, State: domain.LandingPending}
	if _, err := coordinator.refs.CreateLandingIntent(context.Background(), intent); err != nil {
		t.Fatalf("CreateLandingIntent() error = %v", err)
	}
	if _, err := coordinator.refs.TransitionLanding(context.Background(), intent.ID, domain.LandingPending, domain.LandingRunning); err != nil {
		t.Fatalf("TransitionLanding(running) error = %v", err)
	}
	intent.FinalPlanID = strings.Repeat("f", 64)
	if err := coordinator.refs.SetLandingPlan(context.Background(), intent.ID, intent.FinalPlanID); err != nil {
		t.Fatalf("SetLandingPlan() error = %v", err)
	}
	if _, err := coordinator.refs.BlockLanding(context.Background(), intent.ID, domain.LandingRunning, domain.CodeValidation); err != nil {
		t.Fatalf("BlockLanding() error = %v", err)
	}
	if _, err := coordinator.refs.TransitionLanding(context.Background(), intent.ID, domain.LandingBlocked, domain.LandingRecovering); err != nil {
		t.Fatalf("TransitionLanding(recovering) error = %v", err)
	}
	object := domain.ContextObject{SchemaVersion: 4, RepositoryID: intent.RepositoryID, RefName: intent.TargetRef, ParentIDs: []string{current.CommitID}, SourceSHA: mergeSHA, Message: "manual recovery", Author: "tester", CreatedAt: time.Now().UTC(), Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: mergeSHA}}, LandingReceipts: []domain.LandingReceipt{{ID: intent.ID, Provider: intent.Change.Provider, ForgeRepository: intent.Change.Repository, ChangeNumber: intent.Change.Number, ContextRepositoryID: intent.RepositoryID, TargetRef: intent.TargetRef, FinalPlanID: intent.FinalPlanID, SourceMergeSHA: mergeSHA, BaseContextCommitID: current.CommitID, Resolver: "manual"}}}
	contents, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("Marshal(object) error = %v", err)
	}
	digest := blake3.Sum256(contents)
	objectID := formatDigest(digest[:])
	if _, err := storage.PublishObject(context.Background(), intent.RepositoryID, objectID, contents); err != nil {
		t.Fatalf("PublishObject() error = %v", err)
	}
	if _, err := storage.PublishObject(context.Background(), "other-context-repo", objectID, contents); err != nil {
		t.Fatalf("PublishObject(other route) error = %v", err)
	}
	next := remote.Ref{RefName: current.RefName, CommitID: objectID, SourceSHA: mergeSHA, Version: current.Version + 1}
	payload, _ := json.Marshal(casRequest{Expected: current, Next: next})
	server := &Server{store: storage, coordinator: coordinator}
	maliciousRequest := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(payload))
	if err := server.serveRefCAS(httptest.NewRecorder(), maliciousRequest, storage, "other-context-repo", intent.TargetRef); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("serveRefCAS(cross-repository receipt) error = %v, want validation", err)
	}
	unchanged, _ := storage.ReadRef(context.Background(), intent.RepositoryID, intent.TargetRef)
	if unchanged != current {
		t.Fatalf("cross-repository receipt advanced canonical ref: %+v", unchanged)
	}
	request := httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	if err := server.serveRefCAS(response, request, storage, intent.RepositoryID, intent.TargetRef); err != nil {
		t.Fatalf("serveRefCAS() error = %v", err)
	}
	landing, err := coordinator.refs.Landing(context.Background(), intent.ID)
	if err != nil || landing.State != domain.LandingLanded || landing.LandedContextCommitID != objectID {
		t.Fatalf("Landing(after manual CAS) = %+v, %v", landing, err)
	}
}

func (p failingRunner) IndexSource(context.Context, planner.SourceRequest) (planner.SourceEvidence, error) {
	return planner.SourceEvidence{}, p.err
}
