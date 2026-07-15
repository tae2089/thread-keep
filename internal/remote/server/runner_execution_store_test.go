package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
)

func TestRunnerExecutionStorePreparesAndFencesTerminalResult(t *testing.T) {
	store := openTestCoordinatorStore(t)
	claim := claimRunnerExecutionJob(t, store, "runner-owner-1")
	seed := testRunnerExecutionSeed(claim.JobID)
	execution, err := store.PrepareRunnerExecution(context.Background(), claim, seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	if execution.State != RunnerExecutionPrepared || execution.RunnerAttempt != 1 || execution.ClaimFencingToken != claim.FencingToken {
		t.Fatalf("PrepareRunnerExecution() = %+v", execution)
	}
	if err := store.RecordRunnerHandle(context.Background(), claim, execution.ExecutionID, execution.AttemptID, []byte(`{"version":1,"resource":"one"}`)); err != nil {
		t.Fatalf("RecordRunnerHandle() error = %v", err)
	}
	if err := store.CompleteRunnerExecution(context.Background(), claim, execution.ExecutionID, execution.AttemptID, strings.Repeat("e", 64), time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("CompleteRunnerExecution() error = %v", err)
	}
	stale := claim
	stale.FencingToken++
	if err := store.CompleteRunnerExecution(context.Background(), stale, execution.ExecutionID, execution.AttemptID, strings.Repeat("f", 64), time.Now().Add(time.Minute)); domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("CompleteRunnerExecution(stale) error = %v, want concurrent update", err)
	}
	got, err := store.RunnerExecution(context.Background(), execution.ExecutionID)
	if err != nil || got.State != RunnerExecutionSucceeded || got.ResultDigest != strings.Repeat("e", 64) {
		t.Fatalf("RunnerExecution() = %+v, %v", got, err)
	}
}

func TestRunnerExecutionStoreAdoptsActiveAttemptWithoutStaleCancellation(t *testing.T) {
	store := openTestCoordinatorStore(t)
	first := claimRunnerExecutionJob(t, store, "runner-owner-1")
	seed := testRunnerExecutionSeed(first.JobID)
	execution, err := store.PrepareRunnerExecution(context.Background(), first, seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	if err := store.RecordRunnerHandle(context.Background(), first, execution.ExecutionID, execution.AttemptID, []byte(`{"version":1,"resource":"one"}`)); err != nil {
		t.Fatalf("RecordRunnerHandle() error = %v", err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Where("job_id = ?", first.JobID).Updates(map[string]any{"state": CoordinatorJobRetryable, "next_attempt_at": time.Time{}, "lease_until": time.Time{}}).Error; err != nil {
		t.Fatalf("expire first claim error = %v", err)
	}
	var expired coordinatorJobRecord
	if err := store.db.Where("job_id = ?", first.JobID).Take(&expired).Error; err != nil {
		t.Fatalf("read expired claim error = %v", err)
	}
	if expired.State != CoordinatorJobRetryable || !expired.NextAttemptAt.IsZero() {
		t.Fatalf("expired claim = %+v", expired)
	}
	secondJob, ok, err := store.ClaimJob(context.Background(), "runner-owner-2", time.Now().UTC().Add(2*time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds(second) = %+v, %t, %v", secondJob, ok, err)
	}
	second := secondJob.Claim()
	adopted, err := store.AdoptRunnerExecution(context.Background(), second, execution.ExecutionID, execution.AttemptID)
	if err != nil {
		t.Fatalf("AdoptRunnerExecution() error = %v", err)
	}
	if adopted.ClaimOwner != second.LeaseOwner || adopted.ClaimFencingToken != second.FencingToken || adopted.RunnerAttempt != 1 {
		t.Fatalf("AdoptRunnerExecution() = %+v", adopted)
	}
	if err := store.BeginRunnerCancel(context.Background(), first, execution.ExecutionID, execution.AttemptID); domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("BeginRunnerCancel(stale) error = %v, want concurrent update", err)
	}
	if err := store.BeginRunnerCancel(context.Background(), second, execution.ExecutionID, execution.AttemptID); err != nil {
		t.Fatalf("BeginRunnerCancel(current) error = %v", err)
	}
}

func TestRunnerExecutionStoreRecoversExpiredExhaustedAttemptForCleanup(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	job := CoordinatorJob{ID: strings.Repeat("8", 64), DedupeKey: "preview:runner-exhausted", Kind: previewJobKind, Payload: []byte(`{"schema_version":1}`), State: CoordinatorJobPending, MaxAttempts: 1, NextAttemptAt: now}
	if created, err := store.EnqueueJob(context.Background(), job); err != nil || !created {
		t.Fatalf("EnqueueJob() = %t, %v", created, err)
	}
	claimed, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "runner-owner-1", Kinds: []string{previewJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds() = %+v, %t, %v", claimed, ok, err)
	}
	seed := testRunnerExecutionSeed(claimed.ID)
	execution, err := store.PrepareRunnerExecution(context.Background(), claimed.Claim(), seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	if err := store.RecordRunnerHandle(context.Background(), claimed.Claim(), execution.ExecutionID, execution.AttemptID, []byte(`{"version":1,"resource":"one"}`)); err != nil {
		t.Fatalf("RecordRunnerHandle() error = %v", err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Where("job_id = ?", claimed.ID).Update("lease_until", now).Error; err != nil {
		t.Fatalf("expire exhausted claim error = %v", err)
	}

	if next, found, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "runner-owner-2", Kinds: []string{previewJobKind}, LeaseDuration: time.Minute}); err != nil || found {
		t.Fatalf("ClaimJobKinds(after exhaustion) = %+v, %t, %v, want no claim", next, found, err)
	}
	var storedJob coordinatorJobRecord
	if err := store.db.Where("job_id = ?", claimed.ID).Take(&storedJob).Error; err != nil {
		t.Fatalf("read exhausted job error = %v", err)
	}
	if storedJob.State != CoordinatorJobFailed || storedJob.FailureCode != domain.CodeBusy || storedJob.LeaseOwner != "" || !storedJob.LeaseUntil.IsZero() {
		t.Fatalf("exhausted job = %+v, want terminal failed", storedJob)
	}
	storedExecution, err := store.RunnerExecution(context.Background(), execution.ExecutionID)
	if err != nil {
		t.Fatalf("RunnerExecution() error = %v", err)
	}
	if storedExecution.State != RunnerExecutionLost || storedExecution.CleanupState != RunnerCleanupPending || storedExecution.CleanupAfter.IsZero() {
		t.Fatalf("exhausted runner execution = %+v, want lost cleanup-pending", storedExecution)
	}
}

func TestRunnerExecutionStoreRecoversExhaustedFinalLandingAtomically(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	generation := testPRGeneration(1, strings.Repeat("a", 40))
	if changed, err := store.UpsertGeneration(context.Background(), generation); err != nil || !changed {
		t.Fatalf("UpsertGeneration() = %t, %v", changed, err)
	}
	intent := domain.LandingIntent{ID: strings.Repeat("b", 64), RepositoryID: "repo-id", TargetRef: "refs/contexts/main", Change: generation.Change, SourceMergeSHA: strings.Repeat("c", 40), State: domain.LandingPending}
	if created, err := store.CreateLandingIntent(context.Background(), intent); err != nil || !created {
		t.Fatalf("CreateLandingIntent() = %t, %v", created, err)
	}
	if _, err := store.TransitionLanding(context.Background(), intent.ID, domain.LandingPending, domain.LandingRunning); err != nil {
		t.Fatalf("TransitionLanding(running) error = %v", err)
	}
	payload, err := MarshalDurablePayload(finalJobKind, finalJobPayload{RepositoryKey: generation.RepositoryKey, LandingID: intent.ID, Change: generation.Change})
	if err != nil {
		t.Fatalf("MarshalDurablePayload() error = %v", err)
	}
	job := CoordinatorJob{ID: strings.Repeat("d", 64), DedupeKey: "final:runner-exhausted", Kind: finalJobKind, Payload: payload, State: CoordinatorJobPending, MaxAttempts: 1, NextAttemptAt: now}
	if created, err := store.EnqueueJob(context.Background(), job); err != nil || !created {
		t.Fatalf("EnqueueJob() = %t, %v", created, err)
	}
	claimed, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "runner-owner-1", Kinds: []string{finalJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds() = %+v, %t, %v", claimed, ok, err)
	}
	seed := testRunnerExecutionSeed(claimed.ID)
	execution, err := store.PrepareRunnerExecution(context.Background(), claimed.Claim(), seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	if err := store.RecordRunnerHandle(context.Background(), claimed.Claim(), execution.ExecutionID, execution.AttemptID, []byte(`{"version":1,"resource":"one"}`)); err != nil {
		t.Fatalf("RecordRunnerHandle() error = %v", err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Where("job_id = ?", claimed.ID).Update("lease_until", now).Error; err != nil {
		t.Fatalf("expire exhausted final claim error = %v", err)
	}

	if _, found, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "runner-owner-2", Kinds: []string{finalJobKind}, LeaseDuration: time.Minute}); err != nil || found {
		t.Fatalf("ClaimJobKinds(after exhaustion) found = %t, error = %v", found, err)
	}
	var storedJob coordinatorJobRecord
	if err := store.db.Where("job_id = ?", claimed.ID).Take(&storedJob).Error; err != nil {
		t.Fatalf("read recovered final job error = %v", err)
	}
	if storedJob.State != CoordinatorJobDone || !strings.Contains(string(storedJob.Result), `"outcome":"blocked"`) {
		t.Fatalf("recovered final job = %+v, want completed blocked result", storedJob)
	}
	recovered, err := store.Landing(context.Background(), intent.ID)
	if err != nil || recovered.State != domain.LandingBlocked || recovered.AttemptCount != 1 || recovered.LastErrorCode != domain.CodeBusy {
		t.Fatalf("Landing(after recovery) = %+v, %v", recovered, err)
	}
	desired, err := store.DesiredCheck(context.Background(), DesiredCheckLogicalKey(generation.Change, generation.HeadSourceSHA))
	if err != nil || desired.State != "blocked" {
		t.Fatalf("DesiredCheck(after recovery) = %+v, %v", desired, err)
	}
	storedExecution, err := store.RunnerExecution(context.Background(), execution.ExecutionID)
	if err != nil || storedExecution.State != RunnerExecutionLost || storedExecution.CleanupState != RunnerCleanupPending {
		t.Fatalf("RunnerExecution(after final recovery) = %+v, %v", storedExecution, err)
	}
}

func TestRunnerExecutionCleanupFailureRemainsEligibleAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/runner-executions.db"
	store, err := OpenGormRefStore(path)
	if err != nil {
		t.Fatalf("OpenGormRefStore(first) error = %v", err)
	}
	claim := claimRunnerExecutionJob(t, store, "runner-owner-1")
	seed := testRunnerExecutionSeed(claim.JobID)
	execution, err := store.PrepareRunnerExecution(context.Background(), claim, seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	if err := store.FailRunnerExecution(context.Background(), claim, execution.ExecutionID, execution.AttemptID, domain.CodeBusy); err != nil {
		t.Fatalf("FailRunnerExecution() error = %v", err)
	}
	if err := store.MarkRunnerCleanupPending(context.Background(), execution.ExecutionID, execution.AttemptID); err != nil {
		t.Fatalf("MarkRunnerCleanupPending() error = %v", err)
	}
	if err := store.RecordRunnerCleanupFailure(context.Background(), execution.ExecutionID, execution.AttemptID); err != nil {
		t.Fatalf("RecordRunnerCleanupFailure() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	store, err = OpenGormRefStore(path)
	if err != nil {
		t.Fatalf("OpenGormRefStore(second) error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rows, err := store.ListRunnerExecutionsForReconciliation(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].CleanupState != RunnerCleanupFailed || rows[0].CleanupAttempts != 1 {
		t.Fatalf("ListRunnerExecutionsForReconciliation() = %+v, %v", rows, err)
	}
}

func TestRunnerExecutionRetryPreservesPreviousAttemptCleanup(t *testing.T) {
	store := openTestCoordinatorStore(t)
	claim := claimRunnerExecutionJob(t, store, "runner-owner-1")
	seed := testRunnerExecutionSeed(claim.JobID)
	first, err := store.PrepareRunnerExecution(context.Background(), claim, seed)
	if err != nil {
		t.Fatalf("PrepareRunnerExecution() error = %v", err)
	}
	if err := store.FailRunnerExecution(context.Background(), claim, first.ExecutionID, first.AttemptID, domain.CodeBusy); err != nil {
		t.Fatalf("FailRunnerExecution() error = %v", err)
	}
	if err := store.MarkRunnerCleanupPending(context.Background(), first.ExecutionID, first.AttemptID); err != nil {
		t.Fatalf("MarkRunnerCleanupPending() error = %v", err)
	}
	secondAttemptID := strings.Repeat("6", 64)
	second, err := store.AllocateRunnerAttempt(context.Background(), claim, first.ExecutionID, first.AttemptID, secondAttemptID, strings.Repeat("7", 64), "coordinator-instance-1")
	if err != nil {
		t.Fatalf("AllocateRunnerAttempt() error = %v", err)
	}
	if second.RunnerAttempt != 2 || second.AttemptID != secondAttemptID || second.State != RunnerExecutionPrepared {
		t.Fatalf("AllocateRunnerAttempt() = %+v", second)
	}
	var previous runnerExecutionRecord
	if err := store.db.Where("attempt_id = ?", first.AttemptID).Take(&previous).Error; err != nil {
		t.Fatalf("read previous attempt error = %v", err)
	}
	if previous.CleanupState != RunnerCleanupPending || previous.State != RunnerExecutionFailed {
		t.Fatalf("previous attempt = %+v", previous)
	}
}

func TestRunnerExecutionSchemaContainsNoCredentialOrRawRequestColumns(t *testing.T) {
	store := openTestCoordinatorStore(t)
	columns, err := store.db.Migrator().ColumnTypes(&runnerExecutionRecord{})
	if err != nil {
		t.Fatalf("ColumnTypes() error = %v", err)
	}
	for _, column := range columns {
		name := strings.ToLower(column.Name())
		if strings.Contains(name, "credential") || strings.Contains(name, "checkout_token") || strings.Contains(name, "access_token") || strings.Contains(name, "request_payload") {
			t.Fatalf("runner execution schema contains secret/raw request column %q", name)
		}
	}
}

func claimRunnerExecutionJob(t *testing.T, store *GormRefStore, owner string) JobClaim {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	job := CoordinatorJob{ID: strings.Repeat("1", 64), DedupeKey: "preview:runner-execution", Kind: previewJobKind, Payload: []byte(`{"schema_version":1}`), State: CoordinatorJobPending, MaxAttempts: 5, NextAttemptAt: now}
	if created, err := store.EnqueueJob(context.Background(), job); err != nil || !created {
		t.Fatalf("EnqueueJob() = %t, %v", created, err)
	}
	claimed, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: owner, Kinds: []string{previewJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds() = %+v, %t, %v", claimed, ok, err)
	}
	return claimed.Claim()
}

func testRunnerExecutionSeed(jobID string) RunnerExecutionSeed {
	return RunnerExecutionSeed{ExecutionID: strings.Repeat("2", 64), JobID: jobID, RequestDigest: strings.Repeat("3", 64), SpecDigest: strings.Repeat("4", 64), Backend: "docker", AttemptID: strings.Repeat("5", 64), OwnerInstance: "coordinator-instance-1"}
}
