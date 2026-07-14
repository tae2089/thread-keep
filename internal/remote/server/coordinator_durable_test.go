package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/forge"
)

type durablePayloadFixture struct {
	RepositoryKey string `json:"repository_key"`
}

func TestDurablePayloadRejectsUnsupportedVersion(t *testing.T) {
	payload, err := MarshalDurablePayload("process_webhook", durablePayloadFixture{RepositoryKey: "repo"})
	if err != nil {
		t.Fatalf("MarshalDurablePayload() error = %v", err)
	}
	var decoded durablePayloadFixture
	if err := UnmarshalDurablePayload(payload, "process_webhook", &decoded); err != nil || decoded.RepositoryKey != "repo" {
		t.Fatalf("UnmarshalDurablePayload() = %+v, %v", decoded, err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	envelope["schema_version"] = float64(2)
	unsupported, _ := json.Marshal(envelope)
	if err := UnmarshalDurablePayload(unsupported, "process_webhook", &decoded); domain.CodeOf(err) != domain.CodeIncompatiblePayload {
		t.Fatalf("UnmarshalDurablePayload(unsupported) error = %v, want incompatible payload", err)
	}
}

func TestCoordinatorStoreAcceptsWebhookAndProcessJobAtomically(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	payload, err := MarshalDurablePayload("process_webhook", durablePayloadFixture{RepositoryKey: "repo"})
	if err != nil {
		t.Fatalf("MarshalDurablePayload() error = %v", err)
	}
	accept := WebhookAccept{
		Delivery:     WebhookDelivery{Provider: "github", DeliveryID: "delivery-atomic", PayloadHash: strings.Repeat("a", 64), ReceivedAt: now},
		EventPayload: []byte(`{"provider":"github","delivery_id":"delivery-atomic"}`),
		Job:          CoordinatorJob{ID: strings.Repeat("b", 64), DedupeKey: "webhook:github:delivery-atomic", Kind: processWebhookJobKind, Payload: payload, State: CoordinatorJobPending, MaxAttempts: 5, NextAttemptAt: now},
	}
	accepted, err := store.AcceptWebhook(context.Background(), accept)
	if err != nil || !accepted {
		t.Fatalf("AcceptWebhook(first) = %t, %v", accepted, err)
	}
	accepted, err = store.AcceptWebhook(context.Background(), accept)
	if err != nil || accepted {
		t.Fatalf("AcceptWebhook(duplicate) = %t, %v", accepted, err)
	}
	var deliveryCount, jobCount int64
	if err := store.db.Model(&webhookDeliveryRecord{}).Count(&deliveryCount).Error; err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Count(&jobCount).Error; err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if deliveryCount != 1 || jobCount != 1 {
		t.Fatalf("atomic counts = deliveries %d jobs %d, want 1 and 1", deliveryCount, jobCount)
	}
	conflict := accept
	conflict.Delivery.PayloadHash = strings.Repeat("c", 64)
	if _, err := store.AcceptWebhook(context.Background(), conflict); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("AcceptWebhook(hash conflict) error = %v, want validation", err)
	}
	invalid := accept
	invalid.Delivery.DeliveryID = "delivery-rollback"
	invalid.Job.ID = strings.Repeat("d", 64)
	invalid.Job.DedupeKey = "webhook:github:delivery-rollback"
	invalid.Job.MaxAttempts = 0
	if _, err := store.AcceptWebhook(context.Background(), invalid); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("AcceptWebhook(invalid job) error = %v, want validation", err)
	}
	if err := store.db.Model(&webhookDeliveryRecord{}).Where("delivery_id = ?", "delivery-rollback").Count(&deliveryCount).Error; err != nil || deliveryCount != 0 {
		t.Fatalf("rolled back delivery count = %d, %v, want 0", deliveryCount, err)
	}
}

func TestCoordinatorStoreClaimsByKindPriorityAndFencesStaleCompletion(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	for _, job := range []CoordinatorJob{
		{ID: strings.Repeat("e", 64), DedupeKey: "preview:repo:42:1", Kind: previewJobKind, Priority: 100, Payload: []byte(`{"schema_version":1}`), State: CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: now},
		{ID: strings.Repeat("f", 64), DedupeKey: "final:repo:42", Kind: finalJobKind, Priority: 200, Payload: []byte(`{"schema_version":1}`), State: CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: now},
		{ID: strings.Repeat("a", 64), DedupeKey: "check:repo:42", Kind: checkJobKind, Priority: 300, Payload: []byte(`{"schema_version":1}`), State: CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: now},
	} {
		if _, err := store.EnqueueJob(context.Background(), job); err != nil {
			t.Fatalf("EnqueueJob(%s) error = %v", job.Kind, err)
		}
	}
	claimed, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "planner-1", Kinds: []string{previewJobKind, finalJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok || claimed.Kind != finalJobKind || claimed.FencingToken != 1 {
		t.Fatalf("ClaimJobKinds() = %+v, %t, %v, want final fence 1", claimed, ok, err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Where("job_id = ?", claimed.ID).Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	reclaimed, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "planner-2", Kinds: []string{previewJobKind, finalJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok || reclaimed.ID != claimed.ID || reclaimed.FencingToken != 2 {
		t.Fatalf("ClaimJobKinds(reclaim) = %+v, %t, %v, want same job fence 2", reclaimed, ok, err)
	}
	if err := store.CompleteClaim(context.Background(), claimed.Claim(), []byte(`{"outcome":"stale"}`)); domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("CompleteClaim(stale) error = %v, want concurrent update", err)
	}
	if err := store.CompleteClaim(context.Background(), reclaimed.Claim(), []byte(`{"outcome":"done"}`)); err != nil {
		t.Fatalf("CompleteClaim(current) error = %v", err)
	}
}

func TestCoordinatorStoreFencesStaleFailureWithReusedWorkerID(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	job := CoordinatorJob{ID: strings.Repeat("7", 64), DedupeKey: "preview:repo:42:failure-fence", Kind: previewJobKind, Priority: 100, Payload: []byte(`{"schema_version":1}`), State: CoordinatorJobPending, MaxAttempts: 3, NextAttemptAt: now}
	if _, err := store.EnqueueJob(context.Background(), job); err != nil {
		t.Fatalf("EnqueueJob() error = %v", err)
	}
	first, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "stable-worker", Kinds: []string{previewJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds(first) = %+v, %t, %v", first, ok, err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Where("job_id = ?", first.ID).Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	current, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "stable-worker", Kinds: []string{previewJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok || current.FencingToken != first.FencingToken+1 {
		t.Fatalf("ClaimJobKinds(reclaim) = %+v, %t, %v", current, ok, err)
	}
	if err := store.FailClaim(context.Background(), first.Claim()); domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("FailClaim(stale) error = %v, want concurrent update", err)
	}
	if err := store.CompleteClaim(context.Background(), current.Claim(), []byte(`{"outcome":"done"}`)); err != nil {
		t.Fatalf("CompleteClaim(current) error = %v", err)
	}
}

func TestCoordinatorStoreAtomicallyBlocksLandingAndCompletesClaim(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	intent := domain.LandingIntent{ID: strings.Repeat("8", 64), RepositoryID: "repo-id", TargetRef: "refs/contexts/main", Change: domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 88}, SourceMergeSHA: strings.Repeat("9", 40), State: domain.LandingPending}
	if created, err := store.CreateLandingIntent(context.Background(), intent); err != nil || !created {
		t.Fatalf("CreateLandingIntent() = %t, %v", created, err)
	}
	job, err := newFinalJob("repo", intent, 3, now)
	if err != nil {
		t.Fatalf("newFinalJob() error = %v", err)
	}
	if created, err := store.EnqueueJob(context.Background(), job); err != nil || !created {
		t.Fatalf("EnqueueJob() = %t, %v", created, err)
	}
	claimed, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "landing-worker", Kinds: []string{finalJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds() = %+v, %t, %v", claimed, ok, err)
	}
	if _, err := store.TransitionLanding(context.Background(), intent.ID, domain.LandingPending, domain.LandingRunning); err != nil {
		t.Fatalf("TransitionLanding() error = %v", err)
	}
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(intent.Change, intent.SourceMergeSHA), Change: intent.Change, HeadSHA: intent.SourceMergeSHA, State: forge.CheckBlocked, Summary: "Context landing is blocked.", UpdatedAt: now}
	staleClaim := claimed.Claim()
	staleClaim.FencingToken++
	input := BlockedLandingCommit{Claim: staleClaim, LandingID: intent.ID, ExpectedState: domain.LandingRunning, ErrorCode: domain.CodeValidation, DesiredCheck: desired, ResultPayload: []byte(`{"outcome":"blocked"}`)}
	if err := store.CommitBlockedLanding(context.Background(), input); domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("CommitBlockedLanding(stale) error = %v, want concurrent update", err)
	}
	unchanged, err := store.Landing(context.Background(), intent.ID)
	if err != nil || unchanged.State != domain.LandingRunning {
		t.Fatalf("Landing(after stale commit) = %+v, %v", unchanged, err)
	}
	if _, err := store.DesiredCheck(context.Background(), desired.LogicalKey); domain.CodeOf(err) != domain.CodeEntityNotFound {
		t.Fatalf("DesiredCheck(after stale commit) error = %v, want not found", err)
	}
	input.Claim = claimed.Claim()
	if err := store.CommitBlockedLanding(context.Background(), input); err != nil {
		t.Fatalf("CommitBlockedLanding(current) error = %v", err)
	}
	blocked, err := store.Landing(context.Background(), intent.ID)
	if err != nil || blocked.State != domain.LandingBlocked || blocked.LastErrorCode != domain.CodeValidation {
		t.Fatalf("Landing(after commit) = %+v, %v", blocked, err)
	}
	published, err := store.DesiredCheck(context.Background(), desired.LogicalKey)
	if err != nil || published.State != forge.CheckBlocked || published.Version != 1 {
		t.Fatalf("DesiredCheck(after commit) = %+v, %v", published, err)
	}
	var storedJob coordinatorJobRecord
	if err := store.db.Where("job_id = ?", claimed.ID).Take(&storedJob).Error; err != nil || storedJob.State != CoordinatorJobDone {
		t.Fatalf("Job(after commit) = %+v, %v", storedJob, err)
	}
}

func TestCoordinatorStoreSchedulesCandidateAndOutboxAtomically(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	change := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}
	first := PRGeneration{RepositoryKey: "repo", Change: change, BaseSourceSHA: strings.Repeat("1", 40), HeadSourceSHA: strings.Repeat("2", 40), Version: 1, UpdatedAt: now}
	if changed, err := store.UpsertGeneration(context.Background(), first); err != nil || !changed {
		t.Fatalf("UpsertGeneration(first) = %t, %v", changed, err)
	}
	delta, err := domain.NormalizeCandidateContextDelta(domain.CandidateContextDelta{SchemaVersion: 2, Change: change, BaseSourceSHA: first.BaseSourceSHA, HeadSourceSHA: first.HeadSourceSHA, BaseContextCommitID: strings.Repeat("3", 64)})
	if err != nil {
		t.Fatalf("NormalizeCandidateContextDelta() error = %v", err)
	}
	digest, err := domain.CandidateContextDigest(delta)
	if err != nil {
		t.Fatalf("CandidateContextDigest() error = %v", err)
	}
	artifact := StoredCandidateArtifact{Digest: digest, Change: change, Delta: delta, CreatedAt: now}
	second := first
	second.CandidateDigest = digest
	second.Version = 2
	second.UpdatedAt = now.Add(time.Second)
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change, second.HeadSourceSHA), Change: change, HeadSHA: second.HeadSourceSHA, State: forge.CheckPlanning, Summary: "Context planning is queued.", UpdatedAt: now}
	invalid := CandidateSchedule{Artifact: artifact, Generation: second, PreviewJob: CoordinatorJob{}, DesiredCheck: desired}
	if _, err := store.ScheduleCandidate(context.Background(), invalid); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ScheduleCandidate(invalid) error = %v, want validation", err)
	}
	if _, err := store.Candidate(context.Background(), digest); domain.CodeOf(err) != domain.CodeEntityNotFound {
		t.Fatalf("Candidate(after rollback) error = %v, want not found", err)
	}
	current, err := store.Generation(context.Background(), "repo", change)
	if err != nil || current.Version != 1 {
		t.Fatalf("Generation(after rollback) = %+v, %v, want version 1", current, err)
	}
	previewJob, err := newPreviewJob(second, now, 3)
	if err != nil {
		t.Fatalf("newPreviewJob() error = %v", err)
	}
	result, err := store.ScheduleCandidate(context.Background(), CandidateSchedule{Artifact: artifact, Generation: second, PreviewJob: previewJob, DesiredCheck: desired})
	if err != nil || !result.ArtifactCreated || !result.GenerationChanged || result.DesiredCheck.Version != 1 {
		t.Fatalf("ScheduleCandidate() = %+v, %v", result, err)
	}
	var previewCount, checkCount, desiredCount int64
	if err := store.db.Model(&coordinatorJobRecord{}).Where("kind = ?", previewJobKind).Count(&previewCount).Error; err != nil {
		t.Fatalf("count preview jobs: %v", err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Where("kind = ?", checkJobKind).Count(&checkCount).Error; err != nil {
		t.Fatalf("count check jobs: %v", err)
	}
	if err := store.db.Model(&desiredCheckRecord{}).Count(&desiredCount).Error; err != nil {
		t.Fatalf("count desired checks: %v", err)
	}
	if previewCount != 1 || checkCount != 1 || desiredCount != 1 {
		t.Fatalf("atomic schedule counts preview=%d check=%d desired=%d, want 1 each", previewCount, checkCount, desiredCount)
	}
	stale := second
	stale.Version = 1
	stale.CandidateDigest = ""
	stale.UpdatedAt = now.Add(2 * time.Second)
	staleJob, err := newPreviewJob(stale, now, 3)
	if err != nil {
		t.Fatalf("newPreviewJob(stale) error = %v", err)
	}
	if _, err := store.ScheduleCandidate(context.Background(), CandidateSchedule{Artifact: artifact, Generation: stale, PreviewJob: staleJob, DesiredCheck: desired}); domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("ScheduleCandidate(stale) error = %v, want concurrent update", err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Where("kind = ?", previewJobKind).Count(&previewCount).Error; err != nil || previewCount != 1 {
		t.Fatalf("preview jobs after stale schedule = %d, %v, want 1", previewCount, err)
	}
}

func TestDesiredCheckOutboxVersionsAndPersistsProviderIdentity(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	change := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 42}
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change, strings.Repeat("a", 40)), Change: change, HeadSHA: strings.Repeat("a", 40), State: forge.CheckPlanning, Summary: "Planning queued.", UpdatedAt: now}
	first, changed, err := store.SetDesiredCheck(context.Background(), desired)
	if err != nil || !changed || first.Version != 1 {
		t.Fatalf("SetDesiredCheck(first) = %+v, %t, %v", first, changed, err)
	}
	duplicate, changed, err := store.SetDesiredCheck(context.Background(), desired)
	if err != nil || changed || duplicate.Version != 1 {
		t.Fatalf("SetDesiredCheck(duplicate) = %+v, %t, %v", duplicate, changed, err)
	}
	desired.State = forge.CheckReady
	desired.Summary = "Plan is ready."
	desired.PlanURL = "/plans/plan-1"
	desired.UpdatedAt = now.Add(time.Second)
	second, changed, err := store.SetDesiredCheck(context.Background(), desired)
	if err != nil || !changed || second.Version != 2 {
		t.Fatalf("SetDesiredCheck(second) = %+v, %t, %v", second, changed, err)
	}
	var checkJobs []coordinatorJobRecord
	if err := store.db.Where("kind = ?", checkJobKind).Find(&checkJobs).Error; err != nil {
		t.Fatalf("list check jobs error = %v", err)
	}
	for _, checkJob := range checkJobs {
		var payload checkJobPayload
		if err := UnmarshalDurablePayload(checkJob.Payload, checkJobKind, &payload); err != nil {
			t.Fatalf("UnmarshalDurablePayload(check job) error = %v", err)
		}
		if payload.Version == first.Version {
			if err := store.db.Model(&coordinatorJobRecord{}).Where("job_id = ?", checkJob.ID).Update("state", CoordinatorJobDone).Error; err != nil {
				t.Fatalf("complete old check job error = %v", err)
			}
		}
	}
	claimed, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "publication-worker", Kinds: []string{checkJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds(check) = %+v, %t, %v", claimed, ok, err)
	}
	if err := store.CommitCheckPublication(context.Background(), CheckPublicationCommit{Claim: claimed.Claim(), LogicalKey: second.LogicalKey, DesiredVersion: second.Version, ProviderCheckRunID: 1234, ResultPayload: []byte(`{"outcome":"published"}`)}); err != nil {
		t.Fatalf("CommitCheckPublication() error = %v", err)
	}
	stored, err := store.DesiredCheck(context.Background(), second.LogicalKey)
	if err != nil || stored.Version != 2 || stored.PublishedVersion != 2 || stored.ProviderCheckRunID != 1234 {
		t.Fatalf("DesiredCheck() = %+v, %v", stored, err)
	}
	var jobs int64
	if err := store.db.Model(&coordinatorJobRecord{}).Where("kind = ?", checkJobKind).Count(&jobs).Error; err != nil || jobs != 2 {
		t.Fatalf("check jobs = %d, %v, want two desired versions", jobs, err)
	}
}

func TestCheckPublicationCommitFencesVersionAndRepairsLatestDesiredState(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	change := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 77}
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change, strings.Repeat("d", 40)), Change: change, HeadSHA: strings.Repeat("d", 40), State: forge.CheckPlanning, Summary: "Planning queued.", UpdatedAt: now}
	first, changed, err := store.SetDesiredCheck(context.Background(), desired)
	if err != nil || !changed {
		t.Fatalf("SetDesiredCheck(first) = %+v, %t, %v", first, changed, err)
	}
	oldJob, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "old-check-worker", Kinds: []string{checkJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds(old) = %+v, %t, %v", oldJob, ok, err)
	}
	desired.State = forge.CheckReady
	desired.Summary = "Plan ready."
	desired.UpdatedAt = now.Add(time.Second)
	second, changed, err := store.SetDesiredCheck(context.Background(), desired)
	if err != nil || !changed || second.Version != 2 {
		t.Fatalf("SetDesiredCheck(second) = %+v, %t, %v", second, changed, err)
	}
	newJob, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "new-check-worker", Kinds: []string{checkJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds(new) = %+v, %t, %v", newJob, ok, err)
	}
	if err := store.CommitCheckPublication(context.Background(), CheckPublicationCommit{Claim: newJob.Claim(), LogicalKey: second.LogicalKey, DesiredVersion: second.Version, ProviderCheckRunID: 9002, ResultPayload: []byte(`{"outcome":"published"}`)}); err != nil {
		t.Fatalf("CommitCheckPublication(new) error = %v", err)
	}
	if err := store.CommitCheckPublication(context.Background(), CheckPublicationCommit{Claim: oldJob.Claim(), LogicalKey: first.LogicalKey, DesiredVersion: first.Version, ProviderCheckRunID: 9001, ResultPayload: []byte(`{"outcome":"published"}`)}); err != nil {
		t.Fatalf("CommitCheckPublication(old) error = %v", err)
	}
	dirty, err := store.DesiredCheck(context.Background(), second.LogicalKey)
	if err != nil || dirty.Version != 2 || dirty.PublishedVersion >= dirty.Version || dirty.ProviderCheckRunID != 9002 {
		t.Fatalf("DesiredCheck(after stale publication) = %+v, %v", dirty, err)
	}
	repairJob, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "repair-check-worker", Kinds: []string{checkJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds(repair) = %+v, %t, %v", repairJob, ok, err)
	}
	var repairPayload checkJobPayload
	if err := UnmarshalDurablePayload(repairJob.Payload, checkJobKind, &repairPayload); err != nil || repairPayload.Version != second.Version {
		t.Fatalf("repair payload = %+v, %v, want version %d", repairPayload, err, second.Version)
	}
}

func TestSupersededCheckClaimRepairsLatestAfterProviderCommitLoss(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	change := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 79}
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change, strings.Repeat("f", 40)), Change: change, HeadSHA: strings.Repeat("f", 40), State: forge.CheckPlanning, Summary: "Planning queued.", UpdatedAt: now}
	first, changed, err := store.SetDesiredCheck(context.Background(), desired)
	if err != nil || !changed {
		t.Fatalf("SetDesiredCheck(first) = %+v, %t, %v", first, changed, err)
	}
	oldJob, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "crashed-check-worker", Kinds: []string{checkJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds(old) = %+v, %t, %v", oldJob, ok, err)
	}
	desired.State = forge.CheckReady
	desired.Summary = "Plan ready."
	desired.UpdatedAt = now.Add(time.Second)
	second, changed, err := store.SetDesiredCheck(context.Background(), desired)
	if err != nil || !changed {
		t.Fatalf("SetDesiredCheck(second) = %+v, %t, %v", second, changed, err)
	}
	newJob, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "latest-check-worker", Kinds: []string{checkJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds(new) = %+v, %t, %v", newJob, ok, err)
	}
	if err := store.CommitCheckPublication(context.Background(), CheckPublicationCommit{Claim: newJob.Claim(), LogicalKey: second.LogicalKey, DesiredVersion: second.Version, ProviderCheckRunID: 9102, ResultPayload: []byte(`{"outcome":"published"}`)}); err != nil {
		t.Fatalf("CommitCheckPublication(new) error = %v", err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Where("job_id = ?", oldJob.ID).Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("expire old check lease error = %v", err)
	}
	reclaimed, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "retry-check-worker", Kinds: []string{checkJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok || reclaimed.ID != oldJob.ID {
		t.Fatalf("ClaimJobKinds(reclaimed old) = %+v, %t, %v", reclaimed, ok, err)
	}
	if err := store.SupersedeCheckClaim(context.Background(), reclaimed.Claim(), first.LogicalKey, first.Version, []byte(`{"outcome":"superseded"}`)); err != nil {
		t.Fatalf("SupersedeCheckClaim() error = %v", err)
	}
	dirty, err := store.DesiredCheck(context.Background(), second.LogicalKey)
	if err != nil || dirty.PublishedVersion >= dirty.Version || dirty.ProviderCheckRunID != 9102 {
		t.Fatalf("DesiredCheck(after superseded retry) = %+v, %v", dirty, err)
	}
	repairJob, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "repair-after-crash", Kinds: []string{checkJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimJobKinds(repair) = %+v, %t, %v", repairJob, ok, err)
	}
	var payload checkJobPayload
	if err := UnmarshalDurablePayload(repairJob.Payload, checkJobKind, &payload); err != nil || payload.Version != second.Version {
		t.Fatalf("repair payload = %+v, %v, want version %d", payload, err, second.Version)
	}
}

func TestDesiredCheckRepairRearmsExhaustedPublicationJob(t *testing.T) {
	store := openTestCoordinatorStore(t)
	now := time.Now().UTC().Add(-time.Minute)
	change := domain.ChangeKey{Provider: "github", Repository: "owner/repository", Number: 78}
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change, strings.Repeat("e", 40)), Change: change, HeadSHA: strings.Repeat("e", 40), State: forge.CheckReady, Summary: "Plan ready.", UpdatedAt: now}
	stored, changed, err := store.SetDesiredCheck(context.Background(), desired)
	if err != nil || !changed {
		t.Fatalf("SetDesiredCheck() = %+v, %t, %v", stored, changed, err)
	}
	job, err := newCheckJob(stored, now, 5)
	if err != nil {
		t.Fatalf("newCheckJob() error = %v", err)
	}
	if err := store.db.Model(&coordinatorJobRecord{}).Where("job_id = ?", job.ID).Updates(map[string]any{"state": CoordinatorJobFailed, "attempts": 5, "next_attempt_at": time.Time{}}).Error; err != nil {
		t.Fatalf("mark check job exhausted error = %v", err)
	}
	repaired, err := store.RepairDesiredChecks(context.Background(), 10)
	if err != nil || repaired != 1 {
		t.Fatalf("RepairDesiredChecks() = %d, %v, want one", repaired, err)
	}
	var repairedJob coordinatorJobRecord
	if err := store.db.Where("job_id = ?", job.ID).Take(&repairedJob).Error; err != nil || repairedJob.State != CoordinatorJobRetryable || repairedJob.Attempts != 0 || repairedJob.NextAttemptAt.IsZero() {
		t.Fatalf("repaired check job = %+v, %v", repairedJob, err)
	}
	claimed, ok, err := store.ClaimJobKinds(context.Background(), JobClaimOptions{WorkerID: "clock-skew-repair-worker", Kinds: []string{checkJobKind}, LeaseDuration: time.Minute})
	if err != nil || !ok || claimed.ID != job.ID {
		t.Fatalf("ClaimJobKinds(repaired with skewed app clock) = %+v, %t, %v", claimed, ok, err)
	}
}

func TestCoordinatorMarksUnsupportedDurablePayloadIncompatible(t *testing.T) {
	coordinator, _, _, _ := newTestCoordinator(t)
	payload, err := MarshalDurablePayload(previewJobKind, previewJobPayload{RepositoryKey: "repo"})
	if err != nil {
		t.Fatalf("MarshalDurablePayload() error = %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	envelope["schema_version"] = float64(2)
	payload, _ = json.Marshal(envelope)
	now := time.Now().UTC()
	job := CoordinatorJob{ID: strings.Repeat("9", 64), DedupeKey: "preview:unsupported", Kind: previewJobKind, Priority: 100, Payload: payload, State: CoordinatorJobPending, MaxAttempts: 5, NextAttemptAt: now}
	if _, err := coordinator.refs.EnqueueJob(context.Background(), job); err != nil {
		t.Fatalf("EnqueueJob() error = %v", err)
	}
	processed, err := coordinator.RunOne(context.Background(), "compat-worker", now)
	if err != nil || !processed {
		t.Fatalf("RunOne() = %t, %v", processed, err)
	}
	var stored coordinatorJobRecord
	if err := coordinator.refs.db.Where("job_id = ?", job.ID).Take(&stored).Error; err != nil {
		t.Fatalf("read job: %v", err)
	}
	if stored.State != CoordinatorJobIncompatible || stored.FailureCode != domain.CodeIncompatiblePayload {
		t.Fatalf("unsupported job = state %s code %s, want incompatible", stored.State, stored.FailureCode)
	}
}

func TestRunnerHeartbeatRejectsOverlappingDurableSingleRunner(t *testing.T) {
	store := openTestCoordinatorStore(t)
	ctx := context.Background()
	first, err := store.AcquireCoordinator(ctx, CoordinatorIdentity{InstanceID: "instance-1", DisplayName: "coordinator"}, time.Minute)
	if err != nil {
		t.Fatalf("AcquireCoordinator(coordinator-1) error = %v", err)
	}
	if _, err := store.AcquireCoordinator(ctx, CoordinatorIdentity{InstanceID: "instance-2", DisplayName: "coordinator"}, time.Minute); domain.CodeOf(err) != domain.CodeBusy {
		t.Fatalf("AcquireCoordinator(overlap) error = %v, want busy", err)
	}
	wrong := first
	wrong.Token = "wrong-token"
	if err := store.ReleaseCoordinator(ctx, wrong); err != nil {
		t.Fatalf("ReleaseCoordinator(non-owner) error = %v", err)
	}
	if err := store.RefreshCoordinator(ctx, first, time.Minute); err != nil {
		t.Fatalf("RefreshCoordinator(owner after non-owner release) error = %v", err)
	}
	if err := store.db.Model(&coordinatorHeartbeatRecord{}).Where("instance_id = ?", first.InstanceID).Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("expire coordinator heartbeat: %v", err)
	}
	second, err := store.AcquireCoordinator(ctx, CoordinatorIdentity{InstanceID: "instance-2", DisplayName: "coordinator"}, time.Minute)
	if err != nil {
		t.Fatalf("AcquireCoordinator(after expiry) error = %v", err)
	}
	if err := store.RefreshCoordinator(ctx, first, time.Minute); domain.CodeOf(err) != domain.CodeBusy {
		t.Fatalf("RefreshCoordinator(stale) error = %v, want busy", err)
	}
	if err := store.ReleaseCoordinator(ctx, first); err != nil {
		t.Fatalf("ReleaseCoordinator(stale) error = %v", err)
	}
	if err := store.RefreshCoordinator(ctx, second, time.Minute); err != nil {
		t.Fatalf("RefreshCoordinator(current after stale release) error = %v", err)
	}
	if err := store.ReleaseCoordinator(ctx, second); err != nil {
		t.Fatalf("ReleaseCoordinator() error = %v", err)
	}
}
