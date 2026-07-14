package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestCoordinatorStoreDeduplicatesWebhookDeliveries(t *testing.T) {
	store := openTestCoordinatorStore(t)
	delivery := WebhookDelivery{Provider: "github", DeliveryID: "delivery-1", PayloadHash: strings.Repeat("a", 64), ReceivedAt: time.Now().UTC()}
	accepted, err := store.AcceptDelivery(context.Background(), delivery)
	if err != nil || !accepted {
		t.Fatalf("AcceptDelivery(first) = %t, %v", accepted, err)
	}
	accepted, err = store.AcceptDelivery(context.Background(), delivery)
	if err != nil || accepted {
		t.Fatalf("AcceptDelivery(duplicate) = %t, %v, want no-op", accepted, err)
	}
	delivery.PayloadHash = strings.Repeat("b", 64)
	if _, err := store.AcceptDelivery(context.Background(), delivery); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("AcceptDelivery(conflicting hash) error = %v, want validation", err)
	}
}

func TestCoordinatorStoreRejectsStaleGenerationPlanPublication(t *testing.T) {
	store := openTestCoordinatorStore(t)
	first := testPRGeneration(1, "head-one")
	changed, err := store.UpsertGeneration(context.Background(), first)
	if err != nil || !changed {
		t.Fatalf("UpsertGeneration(first) = %t, %v", changed, err)
	}
	second := testPRGeneration(2, "head-two")
	changed, err = store.UpsertGeneration(context.Background(), second)
	if err != nil || !changed {
		t.Fatalf("UpsertGeneration(second) = %t, %v", changed, err)
	}
	plan := domain.ContextPlan{SchemaVersion: 1, ID: strings.Repeat("c", 64), Kind: domain.ContextPlanPreview, Fingerprint: domain.PlanFingerprint{RepositoryID: "repo-id", TargetRef: "refs/contexts/main", Change: first.Change}, Outcome: domain.ContextPlanReady, CreatedAt: time.Now().UTC()}
	if err := store.SaveCurrentPlan(context.Background(), first, plan); domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("SaveCurrentPlan(stale) error = %v, want concurrent update", err)
	}
	plan.Fingerprint.Change = second.Change
	if err := store.SaveCurrentPlan(context.Background(), second, plan); err != nil {
		t.Fatalf("SaveCurrentPlan(current) error = %v", err)
	}
	got, err := store.Generation(context.Background(), second.RepositoryKey, second.Change)
	if err != nil || got.CurrentPlanID != plan.ID || got.Version != second.Version {
		t.Fatalf("Generation() = %+v, %v", got, err)
	}
}

func TestCoordinatorStoreClaimsOneLeaseAndReclaimsExpiredJob(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC()
	job := CoordinatorJob{ID: strings.Repeat("d", 64), DedupeKey: "preview:repo:42:1", Kind: "preview_plan", Payload: []byte(`{"generation":1}`), State: CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: now}
	created, err := store.EnqueueJob(context.Background(), job)
	if err != nil || !created {
		t.Fatalf("EnqueueJob() = %t, %v", created, err)
	}
	var wg sync.WaitGroup
	winners := make(chan CoordinatorJob, 2)
	errors := make(chan error, 2)
	for _, worker := range []string{"worker-1", "worker-2"} {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			claimed, ok, err := store.ClaimJob(context.Background(), worker, now, time.Minute)
			if err != nil {
				errors <- err
				return
			}
			if ok {
				winners <- claimed
			}
		}(worker)
	}
	wg.Wait()
	close(winners)
	close(errors)
	for err := range errors {
		t.Fatalf("ClaimJob() error = %v", err)
	}
	if len(winners) != 1 {
		t.Fatalf("ClaimJob() winners = %d, want 1", len(winners))
	}
	claimed, ok, err := store.ClaimJob(context.Background(), "worker-3", now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || claimed.ID != job.ID || claimed.LeaseOwner != "worker-3" || claimed.Attempts != 2 {
		t.Fatalf("ClaimJob(expired) = %+v, %t, %v", claimed, ok, err)
	}
}

func TestCoordinatorStoreBoundsLandingRetriesAndRejectsIllegalTransition(t *testing.T) {
	store := openTestCoordinatorStore(t)
	intent := domain.LandingIntent{ID: strings.Repeat("e", 64), RepositoryID: "repo-id", TargetRef: "refs/contexts/main", Change: domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}, SourceMergeSHA: strings.Repeat("1", 40), State: domain.LandingPending}
	created, err := store.CreateLandingIntent(context.Background(), intent)
	if err != nil || !created {
		t.Fatalf("CreateLandingIntent() = %t, %v", created, err)
	}
	if _, err := store.TransitionLanding(context.Background(), intent.ID, domain.LandingPending, domain.LandingRunning); err != nil {
		t.Fatalf("TransitionLanding(running) error = %v", err)
	}
	first, err := store.RecordLandingFailure(context.Background(), intent.ID, domain.LandingRunning, domain.CodeLocalStorage, time.Now().UTC(), 2)
	if err != nil || first.State != domain.LandingRetryable || first.AttemptCount != 1 {
		t.Fatalf("RecordLandingFailure(first) = %+v, %v", first, err)
	}
	if _, err := store.TransitionLanding(context.Background(), intent.ID, domain.LandingRetryable, domain.LandingRunning); err != nil {
		t.Fatalf("TransitionLanding(retry running) error = %v", err)
	}
	second, err := store.RecordLandingFailure(context.Background(), intent.ID, domain.LandingRunning, domain.CodeLocalStorage, time.Now().UTC(), 2)
	if err != nil || second.State != domain.LandingBlocked || second.AttemptCount != 2 {
		t.Fatalf("RecordLandingFailure(second) = %+v, %v", second, err)
	}
	if _, err := store.TransitionLanding(context.Background(), intent.ID, domain.LandingBlocked, domain.LandingRunning); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("TransitionLanding(illegal) error = %v, want validation", err)
	}
}

func TestCoordinatorStorePersistsIntentAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/coordinator.db"
	store, err := OpenGormRefStore(path)
	if err != nil {
		t.Fatalf("OpenGormRefStore(first) error = %v", err)
	}
	intent := domain.LandingIntent{ID: strings.Repeat("f", 64), RepositoryID: "repo-id", TargetRef: "refs/contexts/main", Change: domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 43}, SourceMergeSHA: strings.Repeat("2", 40), State: domain.LandingPending}
	if created, err := store.CreateLandingIntent(context.Background(), intent); err != nil || !created {
		t.Fatalf("CreateLandingIntent() = %t, %v", created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	store, err = OpenGormRefStore(path)
	if err != nil {
		t.Fatalf("OpenGormRefStore(second) error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	got, err := store.Landing(context.Background(), intent.ID)
	if err != nil || got.ID != intent.ID || got.State != domain.LandingPending {
		t.Fatalf("Landing(reopened) = %+v, %v", got, err)
	}
}

func TestCoordinatorStoreListsLandingsOnlyForBoundRepositoryRef(t *testing.T) {
	store := openTestCoordinatorStore(t)
	for index, refName := range []string{"refs/contexts/main", "refs/contexts/release"} {
		intent := domain.LandingIntent{ID: strings.Repeat(string(rune('a'+index)), 64), RepositoryID: "repo-id", TargetRef: refName, Change: domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42 + index}, SourceMergeSHA: strings.Repeat(string(rune('1'+index)), 40), State: domain.LandingPending}
		if _, err := store.CreateLandingIntent(context.Background(), intent); err != nil {
			t.Fatalf("CreateLandingIntent(%s) error = %v", refName, err)
		}
	}
	landings, err := store.Landings(context.Background(), "repo-id", "refs/contexts/main")
	if err != nil || len(landings) != 1 || landings[0].TargetRef != "refs/contexts/main" {
		t.Fatalf("Landings(main) = %+v, %v", landings, err)
	}
}

func TestCoordinatorStoreRollsBackRejectedPlanPublication(t *testing.T) {
	store := openTestCoordinatorStore(t)
	generation := testPRGeneration(1, "head-one")
	if changed, err := store.UpsertGeneration(context.Background(), generation); err != nil || !changed {
		t.Fatalf("UpsertGeneration() = %t, %v", changed, err)
	}
	plan := domain.ContextPlan{SchemaVersion: 1, ID: strings.Repeat("a", 64), Kind: domain.ContextPlanPreview, Fingerprint: domain.PlanFingerprint{RepositoryID: "repo-id", TargetRef: "refs/contexts/main", Change: generation.Change}, Outcome: domain.ContextPlanReady, CreatedAt: time.Now().UTC()}
	if err := store.SaveCurrentPlan(context.Background(), generation, plan); err != nil {
		t.Fatalf("SaveCurrentPlan(first) error = %v", err)
	}
	plan.Outcome = domain.ContextPlanBlocked
	if err := store.SaveCurrentPlan(context.Background(), generation, plan); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("SaveCurrentPlan(conflict) error = %v, want validation", err)
	}
	got, err := store.Generation(context.Background(), generation.RepositoryKey, generation.Change)
	if err != nil || got.CurrentPlanID != plan.ID {
		t.Fatalf("Generation(after rollback) = %+v, %v", got, err)
	}
	var count int64
	if err := store.db.Model(&contextPlanRecord{}).Where("plan_id = ?", plan.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("context plan count = %d, %v, want 1", count, err)
	}
}

func openTestCoordinatorStore(t *testing.T) *GormRefStore {
	t.Helper()
	store, err := OpenGormRefStore(t.TempDir() + "/coordinator.db")
	if err != nil {
		t.Fatalf("OpenGormRefStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testPRGeneration(version int, head string) PRGeneration {
	return PRGeneration{RepositoryKey: "repo", Change: domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}, BaseSourceSHA: "base", HeadSourceSHA: head, CandidateDigest: "candidate", Version: version, UpdatedAt: time.Now().UTC()}
}
