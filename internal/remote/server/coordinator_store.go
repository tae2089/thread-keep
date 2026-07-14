package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type WebhookDelivery struct {
	Provider    string
	DeliveryID  string
	PayloadHash string
	ReceivedAt  time.Time
}

type WebhookAccept struct {
	Delivery     WebhookDelivery
	EventPayload []byte
	Job          CoordinatorJob
}

type PRGeneration struct {
	RepositoryKey   string
	Change          domain.ChangeKey
	BaseSourceSHA   string
	HeadSourceSHA   string
	CandidateDigest string
	Version         int
	CurrentPlanID   string
	UpdatedAt       time.Time
}

type CoordinatorJobState string

const (
	CoordinatorJobPending      CoordinatorJobState = "pending"
	CoordinatorJobRunning      CoordinatorJobState = "running"
	CoordinatorJobRetryable    CoordinatorJobState = "retryable"
	CoordinatorJobDone         CoordinatorJobState = "done"
	CoordinatorJobFailed       CoordinatorJobState = "failed"
	CoordinatorJobIncompatible CoordinatorJobState = "incompatible"
)

type CoordinatorJob struct {
	ID            string
	DedupeKey     string
	Kind          string
	Priority      int
	Payload       []byte
	Result        []byte
	State         CoordinatorJobState
	Attempts      int
	MaxAttempts   int
	NextAttemptAt time.Time
	LeaseOwner    string
	LeaseUntil    time.Time
	FencingToken  int64
	FailureCode   domain.ErrorCode
}

type JobClaim struct {
	JobID        string
	LeaseOwner   string
	FencingToken int64
}

type JobClaimOptions struct {
	WorkerID      string
	Kinds         []string
	LeaseDuration time.Duration
}

type StoredCandidateArtifact struct {
	Digest    string
	Change    domain.ChangeKey
	Delta     domain.CandidateContextDelta
	CreatedAt time.Time
}

type webhookDeliveryRecord struct {
	Provider     string    `gorm:"primaryKey;column:provider"`
	DeliveryID   string    `gorm:"primaryKey;column:delivery_id"`
	PayloadHash  string    `gorm:"column:payload_hash"`
	ReceivedAt   time.Time `gorm:"column:received_at"`
	EventPayload []byte    `gorm:"column:event_payload"`
}

type prGenerationRecord struct {
	RepositoryKey   string    `gorm:"primaryKey;column:repository_key"`
	Provider        string    `gorm:"primaryKey;column:provider"`
	ForgeRepository string    `gorm:"primaryKey;column:forge_repository"`
	ChangeNumber    int       `gorm:"primaryKey;column:change_number"`
	BaseSourceSHA   string    `gorm:"column:base_source_sha"`
	HeadSourceSHA   string    `gorm:"column:head_source_sha"`
	CandidateDigest string    `gorm:"column:candidate_digest"`
	Version         int       `gorm:"column:version"`
	CurrentPlanID   string    `gorm:"column:current_plan_id"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

type contextPlanRecord struct {
	PlanID          string    `gorm:"primaryKey;column:plan_id"`
	RepositoryKey   string    `gorm:"index;column:repository_key"`
	Provider        string    `gorm:"column:provider"`
	ForgeRepository string    `gorm:"column:forge_repository"`
	ChangeNumber    int       `gorm:"column:change_number"`
	Generation      int       `gorm:"column:generation"`
	Payload         []byte    `gorm:"column:payload"`
	CreatedAt       time.Time `gorm:"column:created_at"`
}

type coordinatorJobRecord struct {
	ID            string              `gorm:"primaryKey;column:job_id"`
	DedupeKey     string              `gorm:"uniqueIndex;column:dedupe_key"`
	Kind          string              `gorm:"column:kind"`
	Priority      int                 `gorm:"index;column:priority"`
	Payload       []byte              `gorm:"column:payload"`
	Result        []byte              `gorm:"column:result"`
	State         CoordinatorJobState `gorm:"index;column:state"`
	Attempts      int                 `gorm:"column:attempts"`
	MaxAttempts   int                 `gorm:"column:max_attempts"`
	NextAttemptAt time.Time           `gorm:"index;column:next_attempt_at"`
	LeaseOwner    string              `gorm:"column:lease_owner"`
	LeaseUntil    time.Time           `gorm:"index;column:lease_until"`
	FencingToken  int64               `gorm:"column:fencing_token"`
	FailureCode   domain.ErrorCode    `gorm:"column:failure_code"`
}

type landingIntentRecord struct {
	ID                    string              `gorm:"primaryKey;column:landing_id"`
	RepositoryID          string              `gorm:"index;column:repository_id"`
	TargetRef             string              `gorm:"column:target_ref"`
	Provider              string              `gorm:"column:provider"`
	ForgeRepository       string              `gorm:"column:forge_repository"`
	ChangeNumber          int                 `gorm:"column:change_number"`
	CandidateDigest       string              `gorm:"column:candidate_digest"`
	SourceMergeSHA        string              `gorm:"column:source_merge_sha"`
	PreviewPlanID         string              `gorm:"column:preview_plan_id"`
	FinalPlanID           string              `gorm:"column:final_plan_id"`
	State                 domain.LandingState `gorm:"index;column:state"`
	AttemptCount          int                 `gorm:"column:attempt_count"`
	NextAttemptAt         time.Time           `gorm:"index;column:next_attempt_at"`
	LastErrorCode         domain.ErrorCode    `gorm:"column:last_error_code"`
	LandedContextCommitID string              `gorm:"column:landed_context_commit_id"`
}

type candidateArtifactRecord struct {
	Digest          string    `gorm:"primaryKey;column:digest"`
	Provider        string    `gorm:"index;column:provider"`
	ForgeRepository string    `gorm:"index;column:forge_repository"`
	ChangeNumber    int       `gorm:"index;column:change_number"`
	Payload         []byte    `gorm:"column:payload"`
	CreatedAt       time.Time `gorm:"column:created_at"`
}

func (j CoordinatorJob) Claim() JobClaim {
	return JobClaim{JobID: j.ID, LeaseOwner: j.LeaseOwner, FencingToken: j.FencingToken}
}

func (webhookDeliveryRecord) TableName() string   { return "webhook_deliveries" }
func (prGenerationRecord) TableName() string      { return "pr_generations" }
func (contextPlanRecord) TableName() string       { return "context_plans" }
func (coordinatorJobRecord) TableName() string    { return "coordinator_jobs" }
func (landingIntentRecord) TableName() string     { return "landing_intents" }
func (candidateArtifactRecord) TableName() string { return "candidate_artifacts" }

func coordinatorModels() []any {
	return []any{&webhookDeliveryRecord{}, &prGenerationRecord{}, &contextPlanRecord{}, &coordinatorJobRecord{}, &landingIntentRecord{}, &candidateArtifactRecord{}, &desiredCheckRecord{}, &coordinatorHeartbeatRecord{}, &landingReceiptRecord{}, &runnerExecutionRecord{}}
}

func (g *GormRefStore) AcceptDelivery(ctx context.Context, delivery WebhookDelivery) (bool, error) {
	if err := validateWebhookDelivery(delivery); err != nil {
		return false, domain.NewError(domain.CodeValidation, errors.New("webhook delivery is incomplete"))
	}
	accepted := false
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		accepted, err = acceptDeliveryTx(tx, delivery, nil)
		return err
	})
	return accepted, err
}

func (g *GormRefStore) AcceptWebhook(ctx context.Context, input WebhookAccept) (bool, error) {
	if err := validateWebhookDelivery(input.Delivery); err != nil || len(input.EventPayload) == 0 {
		return false, domain.NewError(domain.CodeValidation, errors.New("webhook accept input is incomplete"))
	}
	if err := validateCoordinatorJob(input.Job); err != nil || input.Job.Kind != processWebhookJobKind {
		return false, domain.NewError(domain.CodeValidation, errors.New("webhook process job is invalid"))
	}
	accepted := false
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		accepted, err = acceptDeliveryTx(tx, input.Delivery, input.EventPayload)
		if err != nil || !accepted {
			return err
		}
		_, err = enqueueJobTx(tx, input.Job)
		return err
	})
	return accepted, err
}

func (g *GormRefStore) WebhookEvent(ctx context.Context, provider, deliveryID string) ([]byte, error) {
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(deliveryID) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("webhook event key is incomplete"))
	}
	var record webhookDeliveryRecord
	if err := g.db.WithContext(ctx).Where("provider = ? AND delivery_id = ?", provider, deliveryID).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.NewError(domain.CodeEntityNotFound, errors.New("webhook event does not exist"))
		}
		return nil, coordinatorStorageError("read webhook event", err)
	}
	if len(record.EventPayload) == 0 {
		return nil, domain.NewError(domain.CodeValidation, errors.New("webhook delivery has no processable event"))
	}
	return append([]byte(nil), record.EventPayload...), nil
}

func (g *GormRefStore) UpsertGeneration(ctx context.Context, generation PRGeneration) (bool, error) {
	if err := validatePRGeneration(generation); err != nil {
		return false, err
	}
	changed := false
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		changed, err = upsertGenerationTx(tx, generation)
		return err
	})
	return changed, err
}

func (g *GormRefStore) Generation(ctx context.Context, repositoryKey string, change domain.ChangeKey) (PRGeneration, error) {
	var record prGenerationRecord
	if err := generationQuery(g.db.WithContext(ctx), repositoryKey, change).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return PRGeneration{}, domain.NewError(domain.CodeEntityNotFound, errors.New("PR generation does not exist"))
		}
		return PRGeneration{}, coordinatorStorageError("read PR generation", err)
	}
	return prGenerationFrom(record), nil
}

func (g *GormRefStore) SaveCurrentPlan(ctx context.Context, generation PRGeneration, plan domain.ContextPlan) error {
	if err := validatePRGeneration(generation); err != nil {
		return err
	}
	if plan.ID == "" || plan.CreatedAt.IsZero() || plan.Fingerprint.Change != generation.Change {
		return domain.NewError(domain.CodeValidation, errors.New("context plan is incomplete or belongs to another change"))
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		return domain.NewError(domain.CodeValidation, fmt.Errorf("serialize context plan: %w", err))
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return saveCurrentPlanTx(tx, generation, plan, payload) })
}

func (g *GormRefStore) EnqueueJob(ctx context.Context, job CoordinatorJob) (bool, error) {
	if err := validateCoordinatorJob(job); err != nil {
		return false, err
	}
	created := false
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		created, err = enqueueJobTx(tx, job)
		return err
	})
	return created, err
}

func (g *GormRefStore) ClaimJob(ctx context.Context, workerID string, now time.Time, lease time.Duration) (CoordinatorJob, bool, error) {
	if strings.TrimSpace(workerID) == "" || now.IsZero() || lease <= 0 {
		return CoordinatorJob{}, false, domain.NewError(domain.CodeValidation, errors.New("job claim input is invalid"))
	}
	var claimed CoordinatorJob
	found := false
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Where("((state IN ? AND next_attempt_at <= ?) OR (state = ? AND lease_until <= ?)) AND attempts < max_attempts", []CoordinatorJobState{CoordinatorJobPending, CoordinatorJobRetryable}, now, CoordinatorJobRunning, now).Order("priority DESC, next_attempt_at, job_id")
		if g.postgres {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		var record coordinatorJobRecord
		if err := query.Take(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return coordinatorStorageError("claim coordinator job", err)
		}
		result := tx.Model(&coordinatorJobRecord{}).Where("job_id = ? AND attempts = ? AND state = ?", record.ID, record.Attempts, record.State).Updates(map[string]any{"state": CoordinatorJobRunning, "attempts": record.Attempts + 1, "lease_owner": workerID, "lease_until": now.Add(lease), "fencing_token": record.FencingToken + 1})
		if result.Error != nil {
			return coordinatorStorageError("lease coordinator job", result.Error)
		}
		if result.RowsAffected != 1 {
			return nil
		}
		record.State = CoordinatorJobRunning
		record.Attempts++
		record.LeaseOwner = workerID
		record.LeaseUntil = now.Add(lease)
		record.FencingToken++
		claimed = coordinatorJobFrom(record)
		found = true
		return nil
	})
	return claimed, found, err
}

func (g *GormRefStore) ClaimJobKinds(ctx context.Context, options JobClaimOptions) (CoordinatorJob, bool, error) {
	if strings.TrimSpace(options.WorkerID) == "" || len(options.Kinds) == 0 || options.LeaseDuration <= 0 {
		return CoordinatorJob{}, false, domain.NewError(domain.CodeValidation, errors.New("job claim options are invalid"))
	}
	for _, kind := range options.Kinds {
		if strings.TrimSpace(kind) == "" {
			return CoordinatorJob{}, false, domain.NewError(domain.CodeValidation, errors.New("job claim kind is invalid"))
		}
	}
	var claimed CoordinatorJob
	found := false
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		query := tx.Where("kind IN ? AND ((state IN ? AND next_attempt_at <= ?) OR (state = ? AND lease_until <= ?)) AND attempts < max_attempts", options.Kinds, []CoordinatorJobState{CoordinatorJobPending, CoordinatorJobRetryable}, now, CoordinatorJobRunning, now).Order("priority DESC, next_attempt_at, job_id")
		if g.postgres {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		var record coordinatorJobRecord
		if err := query.Take(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return coordinatorStorageError("claim coordinator job by kind", err)
		}
		leaseUntil := now.Add(options.LeaseDuration)
		fencingToken := record.FencingToken + 1
		result := tx.Model(&coordinatorJobRecord{}).Where("job_id = ? AND attempts = ? AND state = ? AND fencing_token = ?", record.ID, record.Attempts, record.State, record.FencingToken).Updates(map[string]any{"state": CoordinatorJobRunning, "attempts": record.Attempts + 1, "lease_owner": options.WorkerID, "lease_until": leaseUntil, "fencing_token": fencingToken})
		if result.Error != nil {
			return coordinatorStorageError("lease coordinator job by kind", result.Error)
		}
		if result.RowsAffected != 1 {
			return nil
		}
		record.State = CoordinatorJobRunning
		record.Attempts++
		record.LeaseOwner = options.WorkerID
		record.LeaseUntil = leaseUntil
		record.FencingToken = fencingToken
		claimed = coordinatorJobFrom(record)
		found = true
		return nil
	})
	return claimed, found, err
}

func (g *GormRefStore) CompleteClaim(ctx context.Context, claim JobClaim, resultPayload []byte) error {
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return completeClaimTx(tx, claim, resultPayload) })
}

func (g *GormRefStore) ValidateClaim(ctx context.Context, claim JobClaim) error {
	if err := validateJobClaim(claim); err != nil {
		return err
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return validateActiveClaimTx(tx, claim) })
}

func (g *GormRefStore) RejectClaim(ctx context.Context, claim JobClaim, code domain.ErrorCode) error {
	if err := validateJobClaim(claim); err != nil || code == "" {
		return domain.NewError(domain.CodeValidation, errors.New("rejected coordinator job claim is incomplete"))
	}
	result := g.db.WithContext(ctx).Model(&coordinatorJobRecord{}).Where("job_id = ? AND state = ? AND lease_owner = ? AND fencing_token = ?", claim.JobID, CoordinatorJobRunning, claim.LeaseOwner, claim.FencingToken).Updates(map[string]any{"state": CoordinatorJobIncompatible, "failure_code": code, "lease_owner": "", "lease_until": time.Time{}})
	if result.Error != nil {
		return coordinatorStorageError("reject incompatible coordinator job", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("coordinator job fencing token is stale"))
	}
	return nil
}

func (g *GormRefStore) FailClaim(ctx context.Context, claim JobClaim) error {
	if err := validateJobClaim(claim); err != nil {
		return err
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return failClaimTx(tx, claim) })
}

func failClaimTx(tx *gorm.DB, claim JobClaim) error {
	now, err := databaseTime(tx)
	if err != nil {
		return err
	}
	var record coordinatorJobRecord
	if err := tx.Where("job_id = ? AND state = ? AND lease_owner = ? AND fencing_token = ?", claim.JobID, CoordinatorJobRunning, claim.LeaseOwner, claim.FencingToken).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("coordinator job fencing token is stale"))
		}
		return coordinatorStorageError("read failed coordinator job", err)
	}
	nextState := CoordinatorJobRetryable
	nextAttempt := now.Add(time.Duration(1<<min(record.Attempts, 10)) * time.Second)
	if record.Attempts >= record.MaxAttempts {
		nextState = CoordinatorJobFailed
		nextAttempt = time.Time{}
	}
	result := tx.Model(&coordinatorJobRecord{}).Where("job_id = ? AND state = ? AND lease_owner = ? AND fencing_token = ?", claim.JobID, CoordinatorJobRunning, claim.LeaseOwner, claim.FencingToken).Updates(map[string]any{"state": nextState, "next_attempt_at": nextAttempt, "lease_owner": "", "lease_until": time.Time{}})
	if result.Error != nil {
		return coordinatorStorageError("release failed coordinator job", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("coordinator job changed while recording failure"))
	}
	return nil
}

func validateActiveClaimTx(tx *gorm.DB, claim JobClaim) error {
	now, err := databaseTime(tx)
	if err != nil {
		return err
	}
	var count int64
	if err := tx.Model(&coordinatorJobRecord{}).Where("job_id = ? AND state = ? AND lease_owner = ? AND fencing_token = ? AND lease_until > ?", claim.JobID, CoordinatorJobRunning, claim.LeaseOwner, claim.FencingToken, now).Count(&count).Error; err != nil {
		return coordinatorStorageError("validate active coordinator claim", err)
	}
	if count != 1 {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("coordinator job claim is stale or expired"))
	}
	return nil
}

func (g *GormRefStore) SaveCandidate(ctx context.Context, artifact StoredCandidateArtifact) (bool, error) {
	payload, err := candidateArtifactPayload(artifact)
	if err != nil {
		return false, err
	}
	created := false
	err = g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		created, err = saveCandidateTx(tx, artifact, payload)
		return err
	})
	return created, err
}

func (g *GormRefStore) Candidate(ctx context.Context, digest string) (StoredCandidateArtifact, error) {
	var record candidateArtifactRecord
	if err := g.db.WithContext(ctx).Where("digest = ?", digest).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return StoredCandidateArtifact{}, domain.NewError(domain.CodeEntityNotFound, errors.New("candidate artifact does not exist"))
		}
		return StoredCandidateArtifact{}, coordinatorStorageError("read candidate artifact", err)
	}
	var delta domain.CandidateContextDelta
	if err := json.Unmarshal(record.Payload, &delta); err != nil {
		return StoredCandidateArtifact{}, domain.NewError(domain.CodeLocalStorage, errors.New("stored candidate artifact is invalid"))
	}
	return StoredCandidateArtifact{Digest: record.Digest, Change: domain.ChangeKey{Provider: record.Provider, Repository: record.ForgeRepository, Number: record.ChangeNumber}, Delta: delta, CreatedAt: record.CreatedAt}, nil
}

func (g *GormRefStore) Plan(ctx context.Context, planID string) (domain.ContextPlan, error) {
	var record contextPlanRecord
	if err := g.db.WithContext(ctx).Where("plan_id = ?", planID).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.ContextPlan{}, domain.NewError(domain.CodeEntityNotFound, errors.New("context plan does not exist"))
		}
		return domain.ContextPlan{}, coordinatorStorageError("read context plan", err)
	}
	var plan domain.ContextPlan
	if err := json.Unmarshal(record.Payload, &plan); err != nil {
		return domain.ContextPlan{}, domain.NewError(domain.CodeLocalStorage, errors.New("stored context plan is invalid"))
	}
	return plan, nil
}

func (g *GormRefStore) CreateLandingIntent(ctx context.Context, intent domain.LandingIntent) (bool, error) {
	if err := validateLandingIntent(intent); err != nil {
		return false, err
	}
	created := false
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		created, err = createLandingIntentTx(tx, intent)
		return err
	})
	return created, err
}

func createLandingIntentTx(tx *gorm.DB, intent domain.LandingIntent) (bool, error) {
	var existing landingIntentRecord
	err := tx.Where("landing_id = ?", intent.ID).Take(&existing).Error
	if err == nil {
		if !sameLandingIdentity(landingIntentFrom(existing), intent) {
			return false, domain.NewError(domain.CodeValidation, errors.New("landing intent ID has different immutable content"))
		}
		return false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, coordinatorStorageError("read landing intent", err)
	}
	record := landingIntentRecordFrom(intent)
	if err := tx.Create(&record).Error; err != nil {
		return false, coordinatorStorageError("create landing intent", err)
	}
	return true, nil
}

func sameLandingIdentity(left, right domain.LandingIntent) bool {
	return left.ID == right.ID && left.RepositoryID == right.RepositoryID && left.TargetRef == right.TargetRef && left.Change == right.Change && left.CandidateDigest == right.CandidateDigest && left.SourceMergeSHA == right.SourceMergeSHA
}

func (g *GormRefStore) Landing(ctx context.Context, id string) (domain.LandingIntent, error) {
	var record landingIntentRecord
	if err := g.db.WithContext(ctx).Where("landing_id = ?", id).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.LandingIntent{}, domain.NewError(domain.CodeEntityNotFound, errors.New("landing intent does not exist"))
		}
		return domain.LandingIntent{}, coordinatorStorageError("read landing intent", err)
	}
	return landingIntentFrom(record), nil
}

func (g *GormRefStore) Landings(ctx context.Context, repositoryID, targetRef string) ([]domain.LandingIntent, error) {
	var records []landingIntentRecord
	if err := g.db.WithContext(ctx).Where("repository_id = ? AND target_ref = ?", repositoryID, targetRef).Order("landing_id").Find(&records).Error; err != nil {
		return nil, coordinatorStorageError("list landing intents", err)
	}
	landings := make([]domain.LandingIntent, 0, len(records))
	for _, record := range records {
		landings = append(landings, landingIntentFrom(record))
	}
	return landings, nil
}

func (g *GormRefStore) SetLandingPlan(ctx context.Context, id, planID string) error {
	if id == "" || planID == "" {
		return domain.NewError(domain.CodeValidation, errors.New("landing plan assignment is invalid"))
	}
	result := g.db.WithContext(ctx).Model(&landingIntentRecord{}).Where("landing_id = ? AND state = ?", id, domain.LandingRunning).Update("final_plan_id", planID)
	if result.Error != nil {
		return coordinatorStorageError("assign final landing plan", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent is not running for plan assignment"))
	}
	return nil
}

func (g *GormRefStore) BlockLanding(ctx context.Context, id string, expected domain.LandingState, code domain.ErrorCode) (domain.LandingIntent, error) {
	if id == "" || code == "" {
		return domain.LandingIntent{}, domain.NewError(domain.CodeValidation, errors.New("blocked landing input is invalid"))
	}
	result := g.db.WithContext(ctx).Model(&landingIntentRecord{}).Where("landing_id = ? AND state = ?", id, expected).Updates(map[string]any{"state": domain.LandingBlocked, "last_error_code": code, "next_attempt_at": time.Time{}})
	if result.Error != nil {
		return domain.LandingIntent{}, coordinatorStorageError("block landing intent", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.LandingIntent{}, domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent state changed before blocking"))
	}
	return g.Landing(ctx, id)
}

func (g *GormRefStore) TransitionLanding(ctx context.Context, id string, expected, next domain.LandingState) (domain.LandingIntent, error) {
	if !validLandingTransition(expected, next) {
		return domain.LandingIntent{}, domain.NewError(domain.CodeValidation, fmt.Errorf("landing transition %s -> %s is not allowed", expected, next))
	}
	result := g.db.WithContext(ctx).Model(&landingIntentRecord{}).Where("landing_id = ? AND state = ?", id, expected).Update("state", next)
	if result.Error != nil {
		return domain.LandingIntent{}, coordinatorStorageError("transition landing intent", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.LandingIntent{}, domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent state changed concurrently"))
	}
	return g.Landing(ctx, id)
}

func (g *GormRefStore) RecordLandingFailure(ctx context.Context, id string, expected domain.LandingState, code domain.ErrorCode, now time.Time, maxAttempts int) (domain.LandingIntent, error) {
	if expected != domain.LandingRunning || code == "" || now.IsZero() || maxAttempts < 1 {
		return domain.LandingIntent{}, domain.NewError(domain.CodeValidation, errors.New("landing failure input is invalid"))
	}
	var updated domain.LandingIntent
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record landingIntentRecord
		if err := tx.Where("landing_id = ?", id).Take(&record).Error; err != nil {
			return coordinatorStorageError("read landing intent before failure", err)
		}
		if record.State != expected {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent state changed before failure recording"))
		}
		record.AttemptCount++
		record.LastErrorCode = code
		if record.AttemptCount >= maxAttempts {
			record.State = domain.LandingBlocked
			record.NextAttemptAt = time.Time{}
		} else {
			record.State = domain.LandingRetryable
			record.NextAttemptAt = now.Add(time.Duration(1<<min(record.AttemptCount, 10)) * time.Second)
		}
		result := tx.Model(&landingIntentRecord{}).Where("landing_id = ? AND state = ? AND attempt_count = ?", id, expected, record.AttemptCount-1).Updates(map[string]any{"state": record.State, "attempt_count": record.AttemptCount, "next_attempt_at": record.NextAttemptAt, "last_error_code": record.LastErrorCode})
		if result.Error != nil {
			return coordinatorStorageError("record landing failure", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent changed while recording failure"))
		}
		updated = landingIntentFrom(record)
		return nil
	})
	return updated, err
}

func validatePRGeneration(generation PRGeneration) error {
	if strings.TrimSpace(generation.RepositoryKey) == "" || generation.Change.Provider == "" || generation.Change.Repository == "" || generation.Change.Number < 1 || generation.BaseSourceSHA == "" || generation.HeadSourceSHA == "" || generation.Version < 1 || generation.UpdatedAt.IsZero() {
		return domain.NewError(domain.CodeValidation, errors.New("PR generation is incomplete"))
	}
	return nil
}

func validateLandingIntent(intent domain.LandingIntent) error {
	if intent.ID == "" || intent.RepositoryID == "" || intent.TargetRef == "" || intent.Change.Provider == "" || intent.Change.Repository == "" || intent.Change.Number < 1 || intent.SourceMergeSHA == "" || intent.State != domain.LandingPending || intent.AttemptCount != 0 {
		return domain.NewError(domain.CodeValidation, errors.New("landing intent is incomplete or not pending"))
	}
	return nil
}

func validLandingTransition(from, to domain.LandingState) bool {
	switch from {
	case domain.LandingPending:
		return to == domain.LandingRunning || to == domain.LandingLanded || to == domain.LandingBlocked
	case domain.LandingRunning:
		return to == domain.LandingLanded || to == domain.LandingRetryable || to == domain.LandingBlocked
	case domain.LandingRetryable:
		return to == domain.LandingRunning || to == domain.LandingBlocked
	case domain.LandingBlocked:
		return to == domain.LandingRecovering
	case domain.LandingRecovering:
		return to == domain.LandingBlocked || to == domain.LandingLanded
	default:
		return false
	}
}

func generationQuery(db *gorm.DB, repositoryKey string, change domain.ChangeKey) *gorm.DB {
	return db.Where("repository_key = ? AND provider = ? AND forge_repository = ? AND change_number = ?", repositoryKey, change.Provider, change.Repository, change.Number)
}

func prGenerationRecordFrom(generation PRGeneration) *prGenerationRecord {
	return &prGenerationRecord{RepositoryKey: generation.RepositoryKey, Provider: generation.Change.Provider, ForgeRepository: generation.Change.Repository, ChangeNumber: generation.Change.Number, BaseSourceSHA: generation.BaseSourceSHA, HeadSourceSHA: generation.HeadSourceSHA, CandidateDigest: generation.CandidateDigest, Version: generation.Version, CurrentPlanID: generation.CurrentPlanID, UpdatedAt: generation.UpdatedAt.UTC()}
}

func prGenerationFrom(record prGenerationRecord) PRGeneration {
	return PRGeneration{RepositoryKey: record.RepositoryKey, Change: domain.ChangeKey{Provider: record.Provider, Repository: record.ForgeRepository, Number: record.ChangeNumber}, BaseSourceSHA: record.BaseSourceSHA, HeadSourceSHA: record.HeadSourceSHA, CandidateDigest: record.CandidateDigest, Version: record.Version, CurrentPlanID: record.CurrentPlanID, UpdatedAt: record.UpdatedAt}
}

func sameGenerationContent(left, right PRGeneration) bool {
	left.UpdatedAt = time.Time{}
	right.UpdatedAt = time.Time{}
	return reflect.DeepEqual(left, right)
}

func upsertGenerationTx(tx *gorm.DB, generation PRGeneration) (bool, error) {
	var existing prGenerationRecord
	err := generationQuery(tx, generation.RepositoryKey, generation.Change).Take(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if generation.Version != 1 {
			return false, domain.NewError(domain.CodeConcurrentUpdate, errors.New("first PR generation must have version one"))
		}
		if err := tx.Create(prGenerationRecordFrom(generation)).Error; err != nil {
			return false, coordinatorStorageError("create PR generation", err)
		}
		return true, nil
	}
	if err != nil {
		return false, coordinatorStorageError("read PR generation", err)
	}
	current := prGenerationFrom(existing)
	if generation.Version == current.Version {
		if !sameGenerationContent(current, generation) {
			return false, domain.NewError(domain.CodeConcurrentUpdate, errors.New("PR generation version has different content"))
		}
		return false, nil
	}
	if generation.Version != current.Version+1 {
		return false, domain.NewError(domain.CodeConcurrentUpdate, errors.New("PR generation version is stale or skips a version"))
	}
	result := generationQuery(tx.Model(&prGenerationRecord{}), generation.RepositoryKey, generation.Change).
		Where("version = ?", current.Version).
		Updates(map[string]any{"base_source_sha": generation.BaseSourceSHA, "head_source_sha": generation.HeadSourceSHA, "candidate_digest": generation.CandidateDigest, "version": generation.Version, "current_plan_id": "", "updated_at": generation.UpdatedAt.UTC()})
	if result.Error != nil {
		return false, coordinatorStorageError("update PR generation", result.Error)
	}
	if result.RowsAffected != 1 {
		return false, domain.NewError(domain.CodeConcurrentUpdate, errors.New("PR generation changed concurrently"))
	}
	return true, nil
}

func saveCurrentPlanTx(tx *gorm.DB, generation PRGeneration, plan domain.ContextPlan, payload []byte) error {
	var current prGenerationRecord
	if err := generationQuery(tx, generation.RepositoryKey, generation.Change).Take(&current).Error; err != nil {
		return coordinatorStorageError("read current PR generation", err)
	}
	if current.Version != generation.Version || current.BaseSourceSHA != generation.BaseSourceSHA || current.HeadSourceSHA != generation.HeadSourceSHA || current.CandidateDigest != generation.CandidateDigest {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("context plan belongs to a stale PR generation"))
	}
	var existing contextPlanRecord
	err := tx.Where("plan_id = ?", plan.ID).Take(&existing).Error
	if err == nil {
		if !reflect.DeepEqual(existing.Payload, payload) {
			return domain.NewError(domain.CodeValidation, errors.New("context plan ID has different immutable content"))
		}
	} else if errors.Is(err, gorm.ErrRecordNotFound) {
		record := contextPlanRecord{PlanID: plan.ID, RepositoryKey: generation.RepositoryKey, Provider: generation.Change.Provider, ForgeRepository: generation.Change.Repository, ChangeNumber: generation.Change.Number, Generation: generation.Version, Payload: payload, CreatedAt: plan.CreatedAt.UTC()}
		if err := tx.Create(&record).Error; err != nil {
			return coordinatorStorageError("create context plan", err)
		}
	} else {
		return coordinatorStorageError("read context plan", err)
	}
	result := generationQuery(tx.Model(&prGenerationRecord{}), generation.RepositoryKey, generation.Change).Where("version = ?", generation.Version).Update("current_plan_id", plan.ID)
	if result.Error != nil {
		return coordinatorStorageError("publish current context plan", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("PR generation changed before plan publication"))
	}
	return nil
}

func completeClaimTx(tx *gorm.DB, claim JobClaim, resultPayload []byte) error {
	if err := validateJobClaim(claim); err != nil {
		return err
	}
	result := tx.Model(&coordinatorJobRecord{}).Where("job_id = ? AND state = ? AND lease_owner = ? AND fencing_token = ?", claim.JobID, CoordinatorJobRunning, claim.LeaseOwner, claim.FencingToken).Updates(map[string]any{"state": CoordinatorJobDone, "result": resultPayload, "lease_owner": "", "lease_until": time.Time{}})
	if result.Error != nil {
		return coordinatorStorageError("complete fenced coordinator job", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("coordinator job fencing token is stale"))
	}
	return nil
}

func validateJobClaim(claim JobClaim) error {
	if claim.JobID == "" || claim.LeaseOwner == "" || claim.FencingToken < 1 {
		return domain.NewError(domain.CodeValidation, errors.New("coordinator job claim is incomplete"))
	}
	return nil
}

func candidateArtifactPayload(artifact StoredCandidateArtifact) ([]byte, error) {
	normalized, err := domain.NormalizeCandidateContextDelta(artifact.Delta)
	if err != nil {
		return nil, err
	}
	digest, err := domain.CandidateContextDigest(normalized)
	if err != nil || digest != artifact.Digest || artifact.Change != normalized.Change || artifact.CreatedAt.IsZero() {
		return nil, domain.NewError(domain.CodeValidation, errors.New("candidate artifact is incomplete or mismatched"))
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("serialize candidate artifact"))
	}
	return payload, nil
}

func saveCandidateTx(tx *gorm.DB, artifact StoredCandidateArtifact, payload []byte) (bool, error) {
	var existing candidateArtifactRecord
	err := tx.Where("digest = ?", artifact.Digest).Take(&existing).Error
	if err == nil {
		if existing.Provider != artifact.Change.Provider || existing.ForgeRepository != artifact.Change.Repository || existing.ChangeNumber != artifact.Change.Number || !reflect.DeepEqual(existing.Payload, payload) {
			return false, domain.NewError(domain.CodeValidation, errors.New("candidate digest has different immutable content"))
		}
		return false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, coordinatorStorageError("read candidate artifact", err)
	}
	record := candidateArtifactRecord{Digest: artifact.Digest, Provider: artifact.Change.Provider, ForgeRepository: artifact.Change.Repository, ChangeNumber: artifact.Change.Number, Payload: payload, CreatedAt: artifact.CreatedAt.UTC()}
	if err := tx.Create(&record).Error; err != nil {
		return false, coordinatorStorageError("create candidate artifact", err)
	}
	return true, nil
}

func validateWebhookDelivery(delivery WebhookDelivery) error {
	if strings.TrimSpace(delivery.Provider) == "" || strings.TrimSpace(delivery.DeliveryID) == "" || len(delivery.PayloadHash) != 64 || delivery.ReceivedAt.IsZero() {
		return domain.NewError(domain.CodeValidation, errors.New("webhook delivery is incomplete"))
	}
	return nil
}

func acceptDeliveryTx(tx *gorm.DB, delivery WebhookDelivery, eventPayload []byte) (bool, error) {
	record := webhookDeliveryRecord{Provider: delivery.Provider, DeliveryID: delivery.DeliveryID, PayloadHash: delivery.PayloadHash, ReceivedAt: delivery.ReceivedAt.UTC(), EventPayload: append([]byte(nil), eventPayload...)}
	created := tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "provider"}, {Name: "delivery_id"}},
		DoNothing: true,
	}).Create(&record)
	if created.Error != nil {
		return false, coordinatorStorageError("create webhook delivery", created.Error)
	}
	if created.RowsAffected == 1 {
		return true, nil
	}
	var existing webhookDeliveryRecord
	if err := tx.Where("provider = ? AND delivery_id = ?", delivery.Provider, delivery.DeliveryID).Take(&existing).Error; err != nil {
		return false, coordinatorStorageError("read concurrent webhook delivery", err)
	}
	if existing.PayloadHash != delivery.PayloadHash {
		return false, domain.NewError(domain.CodeValidation, errors.New("webhook delivery ID was reused with a different payload"))
	}
	return false, nil
}

func validateCoordinatorJob(job CoordinatorJob) error {
	if job.ID == "" || job.DedupeKey == "" || job.Kind == "" || len(job.Payload) == 0 || job.State != CoordinatorJobPending || job.MaxAttempts < 1 || job.NextAttemptAt.IsZero() || job.Priority < 0 {
		return domain.NewError(domain.CodeValidation, errors.New("coordinator job is incomplete"))
	}
	return nil
}

func enqueueJobTx(tx *gorm.DB, job CoordinatorJob) (bool, error) {
	if err := validateCoordinatorJob(job); err != nil {
		return false, err
	}
	var existing coordinatorJobRecord
	err := tx.Where("dedupe_key = ?", job.DedupeKey).Take(&existing).Error
	if err == nil {
		if existing.ID != job.ID || existing.Kind != job.Kind || existing.Priority != job.Priority || !reflect.DeepEqual(existing.Payload, job.Payload) {
			return false, domain.NewError(domain.CodeValidation, errors.New("coordinator job dedupe key has different content"))
		}
		return false, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, coordinatorStorageError("read coordinator job", err)
	}
	record := coordinatorJobRecordFrom(job)
	if err := tx.Create(&record).Error; err != nil {
		return false, coordinatorStorageError("create coordinator job", err)
	}
	return true, nil
}

func databaseTime(tx *gorm.DB) (time.Time, error) {
	var raw any
	if err := tx.Raw("SELECT CURRENT_TIMESTAMP").Row().Scan(&raw); err != nil {
		return time.Time{}, coordinatorStorageError("read database clock", err)
	}
	switch value := raw.(type) {
	case time.Time:
		return value.UTC(), nil
	case string:
		return parseDatabaseTime(value)
	case []byte:
		return parseDatabaseTime(string(value))
	default:
		return time.Time{}, coordinatorStorageError("read database clock", fmt.Errorf("unsupported database clock value %T", raw))
	}
}

func parseDatabaseTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, coordinatorStorageError("parse database clock", errors.New("database clock format is unsupported"))
}

func coordinatorJobRecordFrom(job CoordinatorJob) coordinatorJobRecord {
	return coordinatorJobRecord{ID: job.ID, DedupeKey: job.DedupeKey, Kind: job.Kind, Priority: job.Priority, Payload: append([]byte(nil), job.Payload...), Result: append([]byte(nil), job.Result...), State: job.State, Attempts: job.Attempts, MaxAttempts: job.MaxAttempts, NextAttemptAt: job.NextAttemptAt.UTC(), LeaseOwner: job.LeaseOwner, LeaseUntil: job.LeaseUntil.UTC(), FencingToken: job.FencingToken, FailureCode: job.FailureCode}
}

func coordinatorJobFrom(record coordinatorJobRecord) CoordinatorJob {
	return CoordinatorJob{ID: record.ID, DedupeKey: record.DedupeKey, Kind: record.Kind, Priority: record.Priority, Payload: append([]byte(nil), record.Payload...), Result: append([]byte(nil), record.Result...), State: record.State, Attempts: record.Attempts, MaxAttempts: record.MaxAttempts, NextAttemptAt: record.NextAttemptAt, LeaseOwner: record.LeaseOwner, LeaseUntil: record.LeaseUntil, FencingToken: record.FencingToken, FailureCode: record.FailureCode}
}

func landingIntentRecordFrom(intent domain.LandingIntent) landingIntentRecord {
	return landingIntentRecord{ID: intent.ID, RepositoryID: intent.RepositoryID, TargetRef: intent.TargetRef, Provider: intent.Change.Provider, ForgeRepository: intent.Change.Repository, ChangeNumber: intent.Change.Number, CandidateDigest: intent.CandidateDigest, SourceMergeSHA: intent.SourceMergeSHA, PreviewPlanID: intent.PreviewPlanID, FinalPlanID: intent.FinalPlanID, State: intent.State, AttemptCount: intent.AttemptCount, NextAttemptAt: intent.NextAttemptAt, LastErrorCode: intent.LastErrorCode, LandedContextCommitID: intent.LandedContextCommitID}
}

func landingIntentFrom(record landingIntentRecord) domain.LandingIntent {
	return domain.LandingIntent{ID: record.ID, RepositoryID: record.RepositoryID, TargetRef: record.TargetRef, Change: domain.ChangeKey{Provider: record.Provider, Repository: record.ForgeRepository, Number: record.ChangeNumber}, CandidateDigest: record.CandidateDigest, SourceMergeSHA: record.SourceMergeSHA, PreviewPlanID: record.PreviewPlanID, FinalPlanID: record.FinalPlanID, State: record.State, AttemptCount: record.AttemptCount, NextAttemptAt: record.NextAttemptAt, LastErrorCode: record.LastErrorCode, LandedContextCommitID: record.LandedContextCommitID}
}

func coordinatorStorageError(operation string, err error) error {
	return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("%s: %w", operation, err))
}
