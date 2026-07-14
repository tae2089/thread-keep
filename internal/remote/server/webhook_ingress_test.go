package server

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestWebhookIngressDurablyAcceptsWithoutProviderRead(t *testing.T) {
	coordinator, _, fakeForge, _ := newTestCoordinator(t)
	result, err := coordinator.IntakeWebhook(context.Background(), http.Header{"X-Github-Delivery": {"delivery-durable"}}, []byte(`{"event":"fixture"}`))
	if err != nil || !result.Accepted || result.Duplicate || result.Ignored {
		t.Fatalf("IntakeWebhook() = %+v, %v", result, err)
	}
	if calls := fakeForge.getChangeCallCount(); calls != 0 {
		t.Fatalf("GetChange calls during intake = %d, want 0", calls)
	}
	var processJobs int64
	if err := coordinator.refs.db.Model(&coordinatorJobRecord{}).Where("kind = ?", processWebhookJobKind).Count(&processJobs).Error; err != nil || processJobs != 1 {
		t.Fatalf("process webhook jobs = %d, %v, want 1", processJobs, err)
	}
	processed, err := coordinator.RunOne(context.Background(), "control-1", time.Now().UTC())
	if err != nil || !processed {
		t.Fatalf("RunOne(process webhook) = %t, %v", processed, err)
	}
	if calls := fakeForge.getChangeCallCount(); calls != 1 {
		t.Fatalf("GetChange calls after process job = %d, want 1", calls)
	}
	var previewJobs int64
	if err := coordinator.refs.db.Model(&coordinatorJobRecord{}).Where("kind = ?", previewJobKind).Count(&previewJobs).Error; err != nil || previewJobs != 1 {
		t.Fatalf("preview jobs = %d, %v, want 1", previewJobs, err)
	}
}

func TestWebhookIngressRejectsBindingMismatchBeforeProcessJob(t *testing.T) {
	coordinator, _, fakeForge, _ := newTestCoordinator(t)
	fakeForge.mu.Lock()
	fakeForge.change.BaseRef = "release"
	fakeForge.mu.Unlock()
	result, err := coordinator.IntakeWebhook(context.Background(), http.Header{"X-Github-Delivery": {"delivery-wrong-branch"}}, []byte(`{"event":"fixture"}`))
	if err != nil || !result.Accepted || !result.Ignored {
		t.Fatalf("IntakeWebhook(binding mismatch) = %+v, %v", result, err)
	}
	var processJobs int64
	if err := coordinator.refs.db.Model(&coordinatorJobRecord{}).Where("kind = ?", processWebhookJobKind).Count(&processJobs).Error; err != nil || processJobs != 0 {
		t.Fatalf("process webhook jobs = %d, %v, want 0", processJobs, err)
	}
}
