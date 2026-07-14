package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestPostgresCoordinatorDurabilityContracts(t *testing.T) {
	dsn := os.Getenv("THREAD_KEEP_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("THREAD_KEEP_TEST_POSTGRES_DSN is not set")
	}
	first, err := OpenGormRefStore(dsn)
	if err != nil {
		t.Fatalf("OpenGormRefStore(first) error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := OpenGormRefStore(dsn)
	if err != nil {
		t.Fatalf("OpenGormRefStore(second) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	runSuffix := time.Now().UTC().Format("20060102150405.000000000")
	now := time.Now().UTC().Add(-time.Minute)
	deliveryID := "postgres-delivery-" + runSuffix
	payload, err := MarshalDurablePayload(processWebhookJobKind, processWebhookJobPayload{Provider: "github", DeliveryID: deliveryID})
	if err != nil {
		t.Fatalf("MarshalDurablePayload() error = %v", err)
	}
	accept := WebhookAccept{Delivery: WebhookDelivery{Provider: "github", DeliveryID: deliveryID, PayloadHash: strings.Repeat("a", 64), ReceivedAt: now}, EventPayload: []byte(`{"schema_version":1,"kind":"webhook_event","body":{}}`), Job: CoordinatorJob{ID: postgresTestID("job-" + runSuffix), DedupeKey: "webhook:github:" + deliveryID, Kind: processWebhookJobKind, Priority: 300, Payload: payload, State: CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: now}}
	accepted, err := first.AcceptWebhook(context.Background(), accept)
	if err != nil || !accepted {
		t.Fatalf("AcceptWebhook() = %t, %v", accepted, err)
	}
	claimed, ok, err := second.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "postgres-worker", Kinds: []string{processWebhookJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok || claimed.FencingToken != 1 {
		t.Fatalf("ClaimJobKinds() = %+v, %t, %v", claimed, ok, err)
	}
	if err := second.CompleteClaim(context.Background(), claimed.Claim(), []byte(`{"outcome":"done"}`)); err != nil {
		t.Fatalf("CompleteClaim() error = %v", err)
	}
	raceDeliveryID := "postgres-race-delivery-" + runSuffix
	racePayload, err := MarshalDurablePayload(processWebhookJobKind, processWebhookJobPayload{Provider: "github", DeliveryID: raceDeliveryID})
	if err != nil {
		t.Fatalf("MarshalDurablePayload(race) error = %v", err)
	}
	raceAccept := WebhookAccept{Delivery: WebhookDelivery{Provider: "github", DeliveryID: raceDeliveryID, PayloadHash: strings.Repeat("c", 64), ReceivedAt: now}, EventPayload: []byte(`{"schema_version":1,"kind":"webhook_event","body":{}}`), Job: CoordinatorJob{ID: postgresTestID("race-job-" + runSuffix), DedupeKey: "webhook:github:" + raceDeliveryID, Kind: processWebhookJobKind, Priority: 300, Payload: racePayload, State: CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: now}}
	type webhookResult struct {
		accepted bool
		err      error
	}
	webhookStart := make(chan struct{})
	webhookResults := make(chan webhookResult, 2)
	var webhookWorkers sync.WaitGroup
	for _, store := range []*GormRefStore{first, second} {
		webhookWorkers.Add(1)
		go func(store *GormRefStore) {
			defer webhookWorkers.Done()
			<-webhookStart
			accepted, err := store.AcceptWebhook(context.Background(), raceAccept)
			webhookResults <- webhookResult{accepted: accepted, err: err}
		}(store)
	}
	close(webhookStart)
	webhookWorkers.Wait()
	close(webhookResults)
	createdDeliveries := 0
	duplicateDeliveries := 0
	for result := range webhookResults {
		if result.err != nil {
			t.Fatalf("AcceptWebhook(concurrent) error = %v", result.err)
		}
		if result.accepted {
			createdDeliveries++
		} else {
			duplicateDeliveries++
		}
	}
	if createdDeliveries != 1 || duplicateDeliveries != 1 {
		t.Fatalf("AcceptWebhook(concurrent) created=%d duplicate=%d, want 1 and 1", createdDeliveries, duplicateDeliveries)
	}
	var processJobs int64
	if err := first.db.Model(&coordinatorJobRecord{}).Where("dedupe_key = ?", raceAccept.Job.DedupeKey).Count(&processJobs).Error; err != nil || processJobs != 1 {
		t.Fatalf("concurrent process jobs = %d, %v, want 1", processJobs, err)
	}

	start := make(chan struct{})
	type acquireResult struct {
		lease CoordinatorLease
		err   error
	}
	results := make(chan acquireResult, 2)
	var workers sync.WaitGroup
	for index, store := range []*GormRefStore{first, second} {
		workers.Add(1)
		go func(index int, store *GormRefStore) {
			defer workers.Done()
			<-start
			lease, err := store.AcquireCoordinator(context.Background(), CoordinatorIdentity{
				InstanceID:  "postgres-instance-" + runSuffix + "-" + string(rune('1'+index)),
				DisplayName: "postgres-coordinator",
			}, time.Minute)
			results <- acquireResult{lease: lease, err: err}
		}(index, store)
	}
	close(start)
	workers.Wait()
	close(results)
	successes := 0
	busy := 0
	for result := range results {
		switch domain.CodeOf(result.err) {
		case "":
			successes++
			if err := first.ReleaseCoordinator(context.Background(), result.lease); err != nil {
				t.Fatalf("ReleaseCoordinator() error = %v", err)
			}
		case domain.CodeBusy:
			busy++
		default:
			t.Fatalf("AcquireCoordinator() unexpected error = %v", result.err)
		}
	}
	if successes != 1 || busy != 1 {
		t.Fatalf("AcquireCoordinator() results successes=%d busy=%d, want 1 and 1", successes, busy)
	}
}

func postgresTestID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(digest[:])
}
