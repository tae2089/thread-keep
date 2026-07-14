package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"gorm.io/gorm"
)

type RunnerExecutionState string

const (
	RunnerExecutionPrepared        RunnerExecutionState = "prepared"
	RunnerExecutionActive          RunnerExecutionState = "active"
	RunnerExecutionCancelRequested RunnerExecutionState = "cancel_requested"
	RunnerExecutionSucceeded       RunnerExecutionState = "succeeded"
	RunnerExecutionFailed          RunnerExecutionState = "failed"
	RunnerExecutionCancelled       RunnerExecutionState = "cancelled"
	RunnerExecutionLost            RunnerExecutionState = "lost"
	RunnerExecutionSuperseded      RunnerExecutionState = "superseded"
)

type RunnerCleanupState string

const (
	RunnerCleanupNone    RunnerCleanupState = "none"
	RunnerCleanupPending RunnerCleanupState = "pending"
	RunnerCleanupCleaned RunnerCleanupState = "cleaned"
	RunnerCleanupFailed  RunnerCleanupState = "failed"
)

const maxRunnerHandleBytes = 64 << 10

type RunnerExecutionSeed struct {
	ExecutionID   string
	JobID         string
	RequestDigest string
	SpecDigest    string
	Backend       string
	AttemptID     string
	OwnerInstance string
}

type RunnerExecution struct {
	ExecutionID       string
	JobID             string
	RequestDigest     string
	SpecDigest        string
	Backend           string
	RunnerAttempt     int
	AttemptID         string
	ClaimOwner        string
	ClaimFencingToken int64
	OwnerInstance     string
	State             RunnerExecutionState
	HandleEnvelope    []byte
	ResultDigest      string
	FailureCode       domain.ErrorCode
	CleanupState      RunnerCleanupState
	CleanupAttempts   int
	CleanupAfter      time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	FinishedAt        time.Time
}

type runnerExecutionRecord struct {
	AttemptID         string               `gorm:"primaryKey;column:attempt_id"`
	ExecutionID       string               `gorm:"uniqueIndex:idx_runner_execution_attempt;index;column:execution_id"`
	JobID             string               `gorm:"index;column:job_id"`
	RequestDigest     string               `gorm:"column:request_digest"`
	SpecDigest        string               `gorm:"column:spec_digest"`
	Backend           string               `gorm:"index;column:backend"`
	RunnerAttempt     int                  `gorm:"uniqueIndex:idx_runner_execution_attempt;column:runner_attempt"`
	ClaimOwner        string               `gorm:"column:claim_owner"`
	ClaimFencingToken int64                `gorm:"column:claim_fencing_token"`
	OwnerInstance     string               `gorm:"column:owner_instance"`
	State             RunnerExecutionState `gorm:"index;column:state"`
	HandleEnvelope    []byte               `gorm:"column:handle_envelope"`
	ResultDigest      string               `gorm:"column:result_digest"`
	FailureCode       domain.ErrorCode     `gorm:"column:failure_code"`
	CleanupState      RunnerCleanupState   `gorm:"index;column:cleanup_state"`
	CleanupAttempts   int                  `gorm:"column:cleanup_attempts"`
	CleanupAfter      time.Time            `gorm:"index;column:cleanup_after"`
	CreatedAt         time.Time            `gorm:"column:created_at"`
	UpdatedAt         time.Time            `gorm:"column:updated_at"`
	FinishedAt        time.Time            `gorm:"column:finished_at"`
}

func (runnerExecutionRecord) TableName() string { return "runner_executions" }

func (g *GormRefStore) PrepareRunnerExecution(ctx context.Context, claim JobClaim, seed RunnerExecutionSeed) (RunnerExecution, error) {
	if err := validateRunnerExecutionSeed(claim, seed); err != nil {
		return RunnerExecution{}, err
	}
	var execution RunnerExecution
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateActiveClaimTx(tx, claim); err != nil {
			return err
		}
		var existing runnerExecutionRecord
		err := tx.Where("execution_id = ?", seed.ExecutionID).Order("runner_attempt DESC").Take(&existing).Error
		if err == nil {
			if !sameRunnerExecutionIdentity(existing, seed) || (existing.RunnerAttempt == 1 && existing.AttemptID != seed.AttemptID) {
				return domain.NewError(domain.CodeValidation, errors.New("runner execution identity has different immutable content"))
			}
			execution = runnerExecutionFrom(existing)
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return coordinatorStorageError("read runner execution", err)
		}
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		record := runnerExecutionRecord{AttemptID: seed.AttemptID, ExecutionID: seed.ExecutionID, JobID: seed.JobID, RequestDigest: seed.RequestDigest, SpecDigest: seed.SpecDigest, Backend: seed.Backend, RunnerAttempt: 1, ClaimOwner: claim.LeaseOwner, ClaimFencingToken: claim.FencingToken, OwnerInstance: seed.OwnerInstance, State: RunnerExecutionPrepared, CleanupState: RunnerCleanupNone, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&record).Error; err != nil {
			return coordinatorStorageError("create runner execution", err)
		}
		execution = runnerExecutionFrom(record)
		return nil
	})
	return execution, err
}

func (g *GormRefStore) RunnerExecution(ctx context.Context, executionID string) (RunnerExecution, error) {
	if strings.TrimSpace(executionID) == "" {
		return RunnerExecution{}, domain.NewError(domain.CodeValidation, errors.New("runner execution ID is required"))
	}
	var record runnerExecutionRecord
	if err := g.db.WithContext(ctx).Where("execution_id = ?", executionID).Order("runner_attempt DESC").Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return RunnerExecution{}, domain.NewError(domain.CodeEntityNotFound, errors.New("runner execution does not exist"))
		}
		return RunnerExecution{}, coordinatorStorageError("read runner execution", err)
	}
	return runnerExecutionFrom(record), nil
}

func (g *GormRefStore) AdoptRunnerExecution(ctx context.Context, claim JobClaim, executionID, attemptID string) (RunnerExecution, error) {
	var execution RunnerExecution
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateActiveClaimTx(tx, claim); err != nil {
			return err
		}
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		result := tx.Model(&runnerExecutionRecord{}).Where("execution_id = ? AND attempt_id = ? AND job_id = ? AND state IN ?", executionID, attemptID, claim.JobID, []RunnerExecutionState{RunnerExecutionPrepared, RunnerExecutionActive, RunnerExecutionSucceeded}).Updates(map[string]any{"claim_owner": claim.LeaseOwner, "claim_fencing_token": claim.FencingToken, "updated_at": now})
		if result.Error != nil {
			return coordinatorStorageError("adopt runner execution", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("runner execution is not adoptable"))
		}
		var record runnerExecutionRecord
		if err := tx.Where("execution_id = ?", executionID).Take(&record).Error; err != nil {
			return coordinatorStorageError("read adopted runner execution", err)
		}
		execution = runnerExecutionFrom(record)
		return nil
	})
	return execution, err
}

func (g *GormRefStore) ReplaceLocalRunnerAttempt(ctx context.Context, claim JobClaim, executionID, previousAttemptID, nextAttemptID, specDigest, ownerInstance string) (RunnerExecution, error) {
	if err := validateJobClaim(claim); err != nil || !validRunnerDigest(nextAttemptID) || !validRunnerDigest(specDigest) || strings.TrimSpace(ownerInstance) == "" {
		return RunnerExecution{}, domain.NewError(domain.CodeValidation, errors.New("local runner attempt replacement is incomplete"))
	}
	var execution RunnerExecution
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateActiveClaimTx(tx, claim); err != nil {
			return err
		}
		var previous runnerExecutionRecord
		if err := tx.Where("execution_id = ? AND attempt_id = ? AND job_id = ? AND backend IN ? AND state IN ?", executionID, previousAttemptID, claim.JobID, []string{"process", "in_process"}, []RunnerExecutionState{RunnerExecutionPrepared, RunnerExecutionActive, RunnerExecutionSucceeded}).Take(&previous).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local runner attempt is not replaceable"))
			}
			return coordinatorStorageError("read local runner attempt", err)
		}
		if previous.OwnerInstance == ownerInstance {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local runner attempt still belongs to this process"))
		}
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		previousUpdates := map[string]any{"cleanup_state": RunnerCleanupPending, "cleanup_after": now, "updated_at": now}
		if previous.State != RunnerExecutionSucceeded {
			previousUpdates["state"] = RunnerExecutionLost
			previousUpdates["finished_at"] = now
		}
		result := tx.Model(&runnerExecutionRecord{}).Where("attempt_id = ? AND state = ?", previous.AttemptID, previous.State).Updates(previousUpdates)
		if result.Error != nil {
			return coordinatorStorageError("mark local runner attempt lost", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("local runner attempt changed concurrently"))
		}
		record := runnerExecutionRecord{AttemptID: nextAttemptID, ExecutionID: previous.ExecutionID, JobID: previous.JobID, RequestDigest: previous.RequestDigest, SpecDigest: specDigest, Backend: previous.Backend, RunnerAttempt: previous.RunnerAttempt + 1, ClaimOwner: claim.LeaseOwner, ClaimFencingToken: claim.FencingToken, OwnerInstance: ownerInstance, State: RunnerExecutionPrepared, CleanupState: RunnerCleanupNone, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&record).Error; err != nil {
			return coordinatorStorageError("replace local runner attempt", err)
		}
		execution = runnerExecutionFrom(record)
		return nil
	})
	return execution, err
}

func (g *GormRefStore) AllocateRunnerAttempt(ctx context.Context, claim JobClaim, executionID, previousAttemptID, nextAttemptID, specDigest, ownerInstance string) (RunnerExecution, error) {
	if err := validateJobClaim(claim); err != nil || !validRunnerDigest(nextAttemptID) || !validRunnerDigest(specDigest) || strings.TrimSpace(ownerInstance) == "" {
		return RunnerExecution{}, domain.NewError(domain.CodeValidation, errors.New("runner attempt allocation is incomplete"))
	}
	var execution RunnerExecution
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateActiveClaimTx(tx, claim); err != nil {
			return err
		}
		var previous runnerExecutionRecord
		if err := tx.Where("execution_id = ? AND attempt_id = ? AND job_id = ? AND state IN ?", executionID, previousAttemptID, claim.JobID, []RunnerExecutionState{RunnerExecutionFailed, RunnerExecutionCancelled, RunnerExecutionLost}).Take(&previous).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.NewError(domain.CodeConcurrentUpdate, errors.New("previous runner attempt is not retryable"))
			}
			return coordinatorStorageError("read previous runner attempt", err)
		}
		var latest runnerExecutionRecord
		if err := tx.Where("execution_id = ?", executionID).Order("runner_attempt DESC").Take(&latest).Error; err != nil {
			return coordinatorStorageError("read latest runner attempt", err)
		}
		if latest.AttemptID != previous.AttemptID {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("runner attempt was already advanced"))
		}
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		record := runnerExecutionRecord{AttemptID: nextAttemptID, ExecutionID: previous.ExecutionID, JobID: previous.JobID, RequestDigest: previous.RequestDigest, SpecDigest: specDigest, Backend: previous.Backend, RunnerAttempt: previous.RunnerAttempt + 1, ClaimOwner: claim.LeaseOwner, ClaimFencingToken: claim.FencingToken, OwnerInstance: ownerInstance, State: RunnerExecutionPrepared, CleanupState: RunnerCleanupNone, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&record).Error; err != nil {
			return coordinatorStorageError("allocate runner attempt", err)
		}
		execution = runnerExecutionFrom(record)
		return nil
	})
	return execution, err
}

func (g *GormRefStore) RecordRunnerHandle(ctx context.Context, claim JobClaim, executionID, attemptID string, handle []byte) error {
	if err := validateRunnerHandle(handle); err != nil {
		return err
	}
	return g.updateClaimedRunnerExecution(ctx, claim, executionID, attemptID, []RunnerExecutionState{RunnerExecutionPrepared, RunnerExecutionActive}, map[string]any{"state": RunnerExecutionActive, "handle_envelope": append([]byte(nil), handle...)}, "record runner handle")
}

func (g *GormRefStore) RecordDiscoveredRunnerHandle(ctx context.Context, executionID, attemptID, backend, specDigest string, handle []byte) error {
	if err := validateRunnerHandle(handle); err != nil {
		return err
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record runnerExecutionRecord
		if err := tx.Where("execution_id = ? AND attempt_id = ? AND backend = ? AND spec_digest = ?", executionID, attemptID, backend, specDigest).Take(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.NewError(domain.CodeConcurrentUpdate, errors.New("runner execution identity changed before handle discovery"))
			}
			return coordinatorStorageError("read runner execution for handle discovery", err)
		}
		if len(record.HandleEnvelope) > 0 {
			if string(record.HandleEnvelope) != string(handle) {
				return domain.NewError(domain.CodeValidation, errors.New("runner execution handle conflicts with discovered resource"))
			}
			return nil
		}
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		result := tx.Model(&runnerExecutionRecord{}).Where("execution_id = ? AND attempt_id = ? AND handle_envelope IS NULL", executionID, attemptID).Updates(map[string]any{"handle_envelope": append([]byte(nil), handle...), "updated_at": now})
		if result.Error != nil {
			return coordinatorStorageError("record discovered runner handle", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("runner execution handle changed concurrently"))
		}
		return nil
	})
}

func (g *GormRefStore) CompleteRunnerExecution(ctx context.Context, claim JobClaim, executionID, attemptID, resultDigest string, cleanupAfter time.Time) error {
	if !validRunnerDigest(resultDigest) || cleanupAfter.IsZero() {
		return domain.NewError(domain.CodeValidation, errors.New("runner result digest is invalid"))
	}
	return g.updateClaimedRunnerExecution(ctx, claim, executionID, attemptID, []RunnerExecutionState{RunnerExecutionActive}, map[string]any{"state": RunnerExecutionSucceeded, "result_digest": resultDigest, "finished_at": gorm.Expr("CURRENT_TIMESTAMP"), "cleanup_state": RunnerCleanupPending, "cleanup_after": cleanupAfter.UTC()}, "complete runner execution")
}

func (g *GormRefStore) FailRunnerExecution(ctx context.Context, claim JobClaim, executionID, attemptID string, code domain.ErrorCode) error {
	if code == "" {
		return domain.NewError(domain.CodeValidation, errors.New("runner failure code is required"))
	}
	return g.updateClaimedRunnerExecution(ctx, claim, executionID, attemptID, []RunnerExecutionState{RunnerExecutionPrepared, RunnerExecutionActive, RunnerExecutionCancelRequested}, map[string]any{"state": RunnerExecutionFailed, "failure_code": code, "finished_at": gorm.Expr("CURRENT_TIMESTAMP")}, "fail runner execution")
}

func (g *GormRefStore) BeginRunnerCancel(ctx context.Context, claim JobClaim, executionID, attemptID string) error {
	return g.updateClaimedRunnerExecution(ctx, claim, executionID, attemptID, []RunnerExecutionState{RunnerExecutionPrepared, RunnerExecutionActive}, map[string]any{"state": RunnerExecutionCancelRequested}, "begin runner cancellation")
}

func (g *GormRefStore) MarkRunnerCancelled(ctx context.Context, claim JobClaim, executionID, attemptID string) error {
	return g.updateClaimedRunnerExecution(ctx, claim, executionID, attemptID, []RunnerExecutionState{RunnerExecutionCancelRequested}, map[string]any{"state": RunnerExecutionCancelled, "finished_at": gorm.Expr("CURRENT_TIMESTAMP"), "cleanup_state": RunnerCleanupPending}, "record runner cancellation")
}

func (g *GormRefStore) MarkRunnerCleanupPending(ctx context.Context, executionID, attemptID string) error {
	result := g.db.WithContext(ctx).Model(&runnerExecutionRecord{}).Where("execution_id = ? AND attempt_id = ? AND state IN ?", executionID, attemptID, terminalRunnerExecutionStates()).Updates(map[string]any{"cleanup_state": RunnerCleanupPending, "cleanup_after": gorm.Expr("CURRENT_TIMESTAMP")})
	if result.Error != nil {
		return coordinatorStorageError("schedule runner cleanup", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("runner execution is not terminal for cleanup"))
	}
	return nil
}

func (g *GormRefStore) RecordRunnerCleanupFailure(ctx context.Context, executionID, attemptID string) error {
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record runnerExecutionRecord
		if err := tx.Where("execution_id = ? AND attempt_id = ? AND cleanup_state IN ?", executionID, attemptID, []RunnerCleanupState{RunnerCleanupPending, RunnerCleanupFailed}).Take(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.NewError(domain.CodeConcurrentUpdate, errors.New("runner cleanup is not pending"))
			}
			return coordinatorStorageError("read failed runner cleanup", err)
		}
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		attempts := record.CleanupAttempts
		if attempts < 1<<30 {
			attempts++
		}
		backoffPower := min(attempts, 10)
		result := tx.Model(&runnerExecutionRecord{}).Where("execution_id = ? AND attempt_id = ? AND cleanup_attempts = ?", executionID, attemptID, record.CleanupAttempts).Updates(map[string]any{"cleanup_state": RunnerCleanupFailed, "cleanup_attempts": attempts, "cleanup_after": now.Add(time.Duration(1<<backoffPower) * time.Second), "updated_at": now})
		if result.Error != nil {
			return coordinatorStorageError("record runner cleanup failure", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("runner cleanup changed concurrently"))
		}
		return nil
	})
}

func (g *GormRefStore) MarkRunnerCleaned(ctx context.Context, executionID, attemptID string) error {
	result := g.db.WithContext(ctx).Model(&runnerExecutionRecord{}).Where("execution_id = ? AND attempt_id = ? AND cleanup_state IN ?", executionID, attemptID, []RunnerCleanupState{RunnerCleanupPending, RunnerCleanupFailed}).Updates(map[string]any{"cleanup_state": RunnerCleanupCleaned, "cleanup_after": time.Time{}, "updated_at": gorm.Expr("CURRENT_TIMESTAMP")})
	if result.Error != nil {
		return coordinatorStorageError("complete runner cleanup", result.Error)
	}
	if result.RowsAffected != 1 {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("runner cleanup is not pending"))
	}
	return nil
}

func (g *GormRefStore) ListRunnerExecutionsForReconciliation(ctx context.Context, limit int) ([]RunnerExecution, error) {
	if limit < 1 || limit > 1000 {
		return nil, domain.NewError(domain.CodeValidation, errors.New("runner reconciliation limit must be between 1 and 1000"))
	}
	var records []runnerExecutionRecord
	err := g.db.WithContext(ctx).Where("state IN ? OR cleanup_state IN ?", []RunnerExecutionState{RunnerExecutionPrepared, RunnerExecutionActive, RunnerExecutionCancelRequested}, []RunnerCleanupState{RunnerCleanupPending, RunnerCleanupFailed}).Order("updated_at, execution_id").Limit(limit).Find(&records).Error
	if err != nil {
		return nil, coordinatorStorageError("list runner executions for reconciliation", err)
	}
	executions := make([]RunnerExecution, 0, len(records))
	for _, record := range records {
		executions = append(executions, runnerExecutionFrom(record))
	}
	return executions, nil
}

func (g *GormRefStore) updateClaimedRunnerExecution(ctx context.Context, claim JobClaim, executionID, attemptID string, states []RunnerExecutionState, updates map[string]any, action string) error {
	if err := validateJobClaim(claim); err != nil || strings.TrimSpace(executionID) == "" || strings.TrimSpace(attemptID) == "" {
		return domain.NewError(domain.CodeValidation, errors.New("claimed runner execution input is incomplete"))
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateActiveClaimTx(tx, claim); err != nil {
			return err
		}
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		updates["updated_at"] = now
		result := tx.Model(&runnerExecutionRecord{}).Where("execution_id = ? AND attempt_id = ? AND job_id = ? AND claim_owner = ? AND claim_fencing_token = ? AND state IN ?", executionID, attemptID, claim.JobID, claim.LeaseOwner, claim.FencingToken, states).Updates(updates)
		if result.Error != nil {
			return coordinatorStorageError(action, result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, fmt.Errorf("%s rejected stale ownership", action))
		}
		return nil
	})
}

func validateRunnerExecutionSeed(claim JobClaim, seed RunnerExecutionSeed) error {
	if err := validateJobClaim(claim); err != nil {
		return err
	}
	if seed.JobID != claim.JobID || !validRunnerDigest(seed.ExecutionID) || !validRunnerDigest(seed.RequestDigest) || !validRunnerDigest(seed.SpecDigest) || !validRunnerDigest(seed.AttemptID) || strings.TrimSpace(seed.OwnerInstance) == "" {
		return domain.NewError(domain.CodeValidation, errors.New("runner execution seed is incomplete"))
	}
	switch seed.Backend {
	case "process", "in_process", "docker", "kubernetes_job":
		return nil
	default:
		return domain.NewError(domain.CodeValidation, errors.New("runner execution backend is invalid"))
	}
}

func validateRunnerHandle(handle []byte) error {
	if len(handle) == 0 || len(handle) > maxRunnerHandleBytes {
		return domain.NewError(domain.CodeValidation, errors.New("runner handle is empty or exceeds the limit"))
	}
	return nil
}

func validRunnerDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func sameRunnerExecutionIdentity(record runnerExecutionRecord, seed RunnerExecutionSeed) bool {
	return record.ExecutionID == seed.ExecutionID && record.JobID == seed.JobID && record.RequestDigest == seed.RequestDigest && record.SpecDigest == seed.SpecDigest && record.Backend == seed.Backend
}

func runnerExecutionFrom(record runnerExecutionRecord) RunnerExecution {
	return RunnerExecution{ExecutionID: record.ExecutionID, JobID: record.JobID, RequestDigest: record.RequestDigest, SpecDigest: record.SpecDigest, Backend: record.Backend, RunnerAttempt: record.RunnerAttempt, AttemptID: record.AttemptID, ClaimOwner: record.ClaimOwner, ClaimFencingToken: record.ClaimFencingToken, OwnerInstance: record.OwnerInstance, State: record.State, HandleEnvelope: append([]byte(nil), record.HandleEnvelope...), ResultDigest: record.ResultDigest, FailureCode: record.FailureCode, CleanupState: record.CleanupState, CleanupAttempts: record.CleanupAttempts, CleanupAfter: record.CleanupAfter, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, FinishedAt: record.FinishedAt}
}

func terminalRunnerExecutionStates() []RunnerExecutionState {
	return []RunnerExecutionState{RunnerExecutionSucceeded, RunnerExecutionFailed, RunnerExecutionCancelled, RunnerExecutionLost, RunnerExecutionSuperseded}
}
