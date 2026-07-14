package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/forge"
	"github.com/zeebo/blake3"
	"gorm.io/gorm"
)

const checkJobPriority = 250

type DesiredCheck struct {
	LogicalKey         string
	Change             domain.ChangeKey
	HeadSHA            string
	State              forge.CheckState
	Summary            string
	PlanURL            string
	Version            int
	PublishedVersion   int
	ProviderCheckRunID int64
	UpdatedAt          time.Time
}

type CheckPublicationCommit struct {
	Claim              JobClaim
	LogicalKey         string
	DesiredVersion     int
	ProviderCheckRunID int64
	ResultPayload      []byte
}

type CandidateSchedule struct {
	Artifact     StoredCandidateArtifact
	Generation   PRGeneration
	PreviewJob   CoordinatorJob
	DesiredCheck DesiredCheck
}

type CandidateScheduleResult struct {
	ArtifactCreated   bool
	GenerationChanged bool
	DesiredCheck      DesiredCheck
}

type PreviewSchedule struct {
	Generation   PRGeneration
	PreviewJob   CoordinatorJob
	DesiredCheck DesiredCheck
}

type LandingSchedule struct {
	Generation   PRGeneration
	Intent       domain.LandingIntent
	FinalJob     CoordinatorJob
	DesiredCheck DesiredCheck
}

type PlanningScheduleResult struct {
	GenerationChanged bool
	LandingCreated    bool
	DesiredCheck      DesiredCheck
}

type BlockedLandingCommit struct {
	Claim         JobClaim
	LandingID     string
	ExpectedState domain.LandingState
	ErrorCode     domain.ErrorCode
	DesiredCheck  DesiredCheck
	ResultPayload []byte
}

type LandingFailureCommit struct {
	Claim         JobClaim
	LandingID     string
	ErrorCode     domain.ErrorCode
	Now           time.Time
	MaxAttempts   int
	BlockedCheck  DesiredCheck
	ResultPayload []byte
}

type desiredCheckRecord struct {
	LogicalKey         string           `gorm:"primaryKey;column:logical_key"`
	Provider           string           `gorm:"index;column:provider"`
	ForgeRepository    string           `gorm:"index;column:forge_repository"`
	ChangeNumber       int              `gorm:"index;column:change_number"`
	HeadSHA            string           `gorm:"index;column:head_sha"`
	State              forge.CheckState `gorm:"column:state"`
	Summary            string           `gorm:"column:summary"`
	PlanURL            string           `gorm:"column:plan_url"`
	Version            int              `gorm:"column:version"`
	PublishedVersion   int              `gorm:"column:published_version"`
	ProviderCheckRunID int64            `gorm:"column:provider_check_run_id"`
	UpdatedAt          time.Time        `gorm:"column:updated_at"`
}

func (desiredCheckRecord) TableName() string { return "desired_checks" }

func DesiredCheckLogicalKey(change domain.ChangeKey, headSHA string) string {
	return change.Provider + ":" + change.Repository + "#" + strconv.Itoa(change.Number) + "@" + strings.ToLower(headSHA)
}

func (g *GormRefStore) SetDesiredCheck(ctx context.Context, desired DesiredCheck) (DesiredCheck, bool, error) {
	if err := validateDesiredCheck(desired); err != nil {
		return DesiredCheck{}, false, err
	}
	var stored DesiredCheck
	changed := false
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		stored, changed, err = upsertDesiredCheckTx(tx, desired)
		if err != nil || !changed {
			return err
		}
		job, err := newCheckJob(stored, desired.UpdatedAt, 5)
		if err != nil {
			return err
		}
		_, err = enqueueJobTx(tx, job)
		return err
	})
	return stored, changed, err
}

func (g *GormRefStore) DesiredCheck(ctx context.Context, logicalKey string) (DesiredCheck, error) {
	if strings.TrimSpace(logicalKey) == "" {
		return DesiredCheck{}, domain.NewError(domain.CodeValidation, errors.New("desired check key is empty"))
	}
	var record desiredCheckRecord
	if err := g.db.WithContext(ctx).Where("logical_key = ?", logicalKey).Take(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return DesiredCheck{}, domain.NewError(domain.CodeEntityNotFound, errors.New("desired check does not exist"))
		}
		return DesiredCheck{}, coordinatorStorageError("read desired check", err)
	}
	return desiredCheckFrom(record), nil
}

func (g *GormRefStore) CommitCheckPublication(ctx context.Context, input CheckPublicationCommit) error {
	if err := validateJobClaim(input.Claim); err != nil || strings.TrimSpace(input.LogicalKey) == "" || input.DesiredVersion < 1 || input.ProviderCheckRunID < 1 {
		return domain.NewError(domain.CodeValidation, errors.New("check publication commit is incomplete"))
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateActiveClaimTx(tx, input.Claim); err != nil {
			return err
		}
		var record desiredCheckRecord
		if err := tx.Where("logical_key = ?", input.LogicalKey).Take(&record).Error; err != nil {
			return coordinatorStorageError("read desired check before publication commit", err)
		}
		if input.DesiredVersion > record.Version {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("check publication is newer than desired state"))
		}
		if input.DesiredVersion < record.Version {
			dirtyVersion := min(record.PublishedVersion, record.Version-1)
			result := tx.Model(&desiredCheckRecord{}).Where("logical_key = ? AND version = ?", input.LogicalKey, record.Version).Update("published_version", dirtyVersion)
			if result.Error != nil {
				return coordinatorStorageError("mark latest desired check dirty", result.Error)
			}
			if result.RowsAffected != 1 {
				return domain.NewError(domain.CodeConcurrentUpdate, errors.New("desired check changed before stale publication commit"))
			}
			record.PublishedVersion = dirtyVersion
			now, err := databaseTime(tx)
			if err != nil {
				return err
			}
			if _, err := ensureCheckJobTx(tx, desiredCheckFrom(record), now, true); err != nil {
				return err
			}
			return completeClaimTx(tx, input.Claim, input.ResultPayload)
		}
		result := tx.Model(&desiredCheckRecord{}).Where("logical_key = ? AND version = ?", input.LogicalKey, input.DesiredVersion).Updates(map[string]any{"provider_check_run_id": input.ProviderCheckRunID, "published_version": input.DesiredVersion})
		if result.Error != nil {
			return coordinatorStorageError("commit check publication", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("desired check changed during publication commit"))
		}
		return completeClaimTx(tx, input.Claim, input.ResultPayload)
	})
}

func (g *GormRefStore) SupersedeCheckClaim(ctx context.Context, claim JobClaim, logicalKey string, staleVersion int, resultPayload []byte) error {
	if err := validateJobClaim(claim); err != nil || strings.TrimSpace(logicalKey) == "" || staleVersion < 1 {
		return domain.NewError(domain.CodeValidation, errors.New("superseded check claim is incomplete"))
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := validateActiveClaimTx(tx, claim); err != nil {
			return err
		}
		var record desiredCheckRecord
		if err := tx.Where("logical_key = ?", logicalKey).Take(&record).Error; err != nil {
			return coordinatorStorageError("read latest desired check for superseded claim", err)
		}
		if staleVersion >= record.Version {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("check claim is not superseded by desired state"))
		}
		dirtyVersion := min(record.PublishedVersion, record.Version-1)
		result := tx.Model(&desiredCheckRecord{}).Where("logical_key = ? AND version = ?", logicalKey, record.Version).Update("published_version", dirtyVersion)
		if result.Error != nil {
			return coordinatorStorageError("mark latest desired check dirty after superseded claim", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("desired check changed before superseded claim completion"))
		}
		record.PublishedVersion = dirtyVersion
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		if _, err := ensureCheckJobTx(tx, desiredCheckFrom(record), now, true); err != nil {
			return err
		}
		return completeClaimTx(tx, claim, resultPayload)
	})
}

func (g *GormRefStore) RepairDesiredChecks(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		return 0, domain.NewError(domain.CodeValidation, errors.New("desired check repair input is invalid"))
	}
	repaired := 0
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		var records []desiredCheckRecord
		if err := tx.Where("published_version < version").Order("updated_at, logical_key").Limit(limit).Find(&records).Error; err != nil {
			return coordinatorStorageError("list desired checks requiring repair", err)
		}
		for _, record := range records {
			changed, err := ensureCheckJobTx(tx, desiredCheckFrom(record), now, true)
			if err != nil {
				return err
			}
			if changed {
				repaired++
			}
		}
		return nil
	})
	return repaired, err
}

func (g *GormRefStore) PublishPreviewResult(ctx context.Context, generation PRGeneration, plan domain.ContextPlan, desired DesiredCheck, job CoordinatorJob, resultPayload []byte) error {
	if err := validatePRGeneration(generation); err != nil {
		return err
	}
	if err := validateDesiredCheck(desired); err != nil {
		return err
	}
	planPayload, err := json.Marshal(plan)
	if err != nil {
		return domain.NewError(domain.CodeValidation, errors.New("serialize context plan"))
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := saveCurrentPlanTx(tx, generation, plan, planPayload); err != nil {
			return err
		}
		stored, changed, err := upsertDesiredCheckTx(tx, desired)
		if err != nil {
			return err
		}
		if changed {
			checkJob, err := newCheckJob(stored, desired.UpdatedAt, 5)
			if err != nil {
				return err
			}
			if _, err := enqueueJobTx(tx, checkJob); err != nil {
				return err
			}
		}
		return completeClaimTx(tx, job.Claim(), resultPayload)
	})
}

func (g *GormRefStore) ScheduleCandidate(ctx context.Context, input CandidateSchedule) (CandidateScheduleResult, error) {
	payload, err := candidateArtifactPayload(input.Artifact)
	if err != nil {
		return CandidateScheduleResult{}, err
	}
	if err := validatePRGeneration(input.Generation); err != nil {
		return CandidateScheduleResult{}, err
	}
	if err := validateCoordinatorJob(input.PreviewJob); err != nil || input.PreviewJob.Kind != previewJobKind {
		return CandidateScheduleResult{}, domain.NewError(domain.CodeValidation, errors.New("candidate preview job is invalid"))
	}
	if err := validateDesiredCheck(input.DesiredCheck); err != nil {
		return CandidateScheduleResult{}, err
	}
	var result CandidateScheduleResult
	err = g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		created, err := saveCandidateTx(tx, input.Artifact, payload)
		if err != nil {
			return err
		}
		changed, err := upsertGenerationTx(tx, input.Generation)
		if err != nil {
			return err
		}
		if changed {
			if _, err := enqueueJobTx(tx, input.PreviewJob); err != nil {
				return err
			}
			desired, desiredChanged, err := upsertDesiredCheckTx(tx, input.DesiredCheck)
			if err != nil {
				return err
			}
			if desiredChanged {
				checkJob, err := newCheckJob(desired, input.DesiredCheck.UpdatedAt, 5)
				if err != nil {
					return err
				}
				if _, err := enqueueJobTx(tx, checkJob); err != nil {
					return err
				}
			}
			result.DesiredCheck = desired
		}
		result.ArtifactCreated = created
		result.GenerationChanged = changed
		return nil
	})
	return result, err
}

func (g *GormRefStore) SchedulePreview(ctx context.Context, input PreviewSchedule) (PlanningScheduleResult, error) {
	if err := validatePRGeneration(input.Generation); err != nil {
		return PlanningScheduleResult{}, err
	}
	if err := validateCoordinatorJob(input.PreviewJob); err != nil || input.PreviewJob.Kind != previewJobKind {
		return PlanningScheduleResult{}, domain.NewError(domain.CodeValidation, errors.New("preview schedule job is invalid"))
	}
	if err := validateDesiredCheck(input.DesiredCheck); err != nil {
		return PlanningScheduleResult{}, err
	}
	var result PlanningScheduleResult
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		changed, err := upsertGenerationTx(tx, input.Generation)
		if err != nil {
			return err
		}
		if changed {
			if _, err := enqueueJobTx(tx, input.PreviewJob); err != nil {
				return err
			}
			stored, desiredChanged, err := upsertDesiredCheckTx(tx, input.DesiredCheck)
			if err != nil {
				return err
			}
			if desiredChanged {
				if err := persistExistingDesiredCheckTx(tx, stored, input.DesiredCheck.UpdatedAt); err != nil {
					return err
				}
			}
			result.DesiredCheck = stored
		}
		result.GenerationChanged = changed
		return nil
	})
	return result, err
}

func (g *GormRefStore) ScheduleLanding(ctx context.Context, input LandingSchedule) (PlanningScheduleResult, error) {
	if err := validatePRGeneration(input.Generation); err != nil {
		return PlanningScheduleResult{}, err
	}
	if err := validateLandingIntent(input.Intent); err != nil {
		return PlanningScheduleResult{}, err
	}
	if err := validateCoordinatorJob(input.FinalJob); err != nil || input.FinalJob.Kind != finalJobKind {
		return PlanningScheduleResult{}, domain.NewError(domain.CodeValidation, errors.New("landing schedule job is invalid"))
	}
	if err := validateDesiredCheck(input.DesiredCheck); err != nil {
		return PlanningScheduleResult{}, err
	}
	var result PlanningScheduleResult
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		generationChanged, err := upsertGenerationTx(tx, input.Generation)
		if err != nil {
			return err
		}
		landingCreated, err := createLandingIntentTx(tx, input.Intent)
		if err != nil {
			return err
		}
		if landingCreated {
			if _, err := enqueueJobTx(tx, input.FinalJob); err != nil {
				return err
			}
			stored, desiredChanged, err := upsertDesiredCheckTx(tx, input.DesiredCheck)
			if err != nil {
				return err
			}
			if desiredChanged {
				if err := persistExistingDesiredCheckTx(tx, stored, input.DesiredCheck.UpdatedAt); err != nil {
					return err
				}
			}
			result.DesiredCheck = stored
		}
		result.GenerationChanged = generationChanged
		result.LandingCreated = landingCreated
		return nil
	})
	return result, err
}

func (g *GormRefStore) CommitBlockedLanding(ctx context.Context, input BlockedLandingCommit) error {
	if err := validateJobClaim(input.Claim); err != nil || input.LandingID == "" || input.ExpectedState != domain.LandingRunning || input.ErrorCode == "" {
		return domain.NewError(domain.CodeValidation, errors.New("blocked landing commit is incomplete"))
	}
	if err := validateDesiredCheck(input.DesiredCheck); err != nil {
		return err
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&landingIntentRecord{}).Where("landing_id = ? AND state = ?", input.LandingID, input.ExpectedState).Updates(map[string]any{
			"state":           domain.LandingBlocked,
			"last_error_code": input.ErrorCode,
			"next_attempt_at": time.Time{},
		})
		if result.Error != nil {
			return coordinatorStorageError("block landing intent in terminal commit", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent changed before terminal commit"))
		}
		if err := persistDesiredCheckTx(tx, input.DesiredCheck); err != nil {
			return err
		}
		return completeClaimTx(tx, input.Claim, input.ResultPayload)
	})
}

func (g *GormRefStore) CommitLandingFailure(ctx context.Context, input LandingFailureCommit) (domain.LandingIntent, error) {
	if err := validateJobClaim(input.Claim); err != nil || input.LandingID == "" || input.ErrorCode == "" || input.Now.IsZero() || input.MaxAttempts < 1 {
		return domain.LandingIntent{}, domain.NewError(domain.CodeValidation, errors.New("landing failure commit is incomplete"))
	}
	if err := validateDesiredCheck(input.BlockedCheck); err != nil {
		return domain.LandingIntent{}, err
	}
	var updated domain.LandingIntent
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record landingIntentRecord
		if err := tx.Where("landing_id = ? AND state = ?", input.LandingID, domain.LandingRunning).Take(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent changed before failure commit"))
			}
			return coordinatorStorageError("read landing intent before failure commit", err)
		}
		record.AttemptCount++
		record.LastErrorCode = input.ErrorCode
		if record.AttemptCount >= input.MaxAttempts {
			record.State = domain.LandingBlocked
			record.NextAttemptAt = time.Time{}
		} else {
			record.State = domain.LandingRetryable
			record.NextAttemptAt = input.Now.UTC().Add(time.Duration(1<<min(record.AttemptCount, 10)) * time.Second)
		}
		result := tx.Model(&landingIntentRecord{}).Where("landing_id = ? AND state = ? AND attempt_count = ?", input.LandingID, domain.LandingRunning, record.AttemptCount-1).Updates(map[string]any{"state": record.State, "attempt_count": record.AttemptCount, "next_attempt_at": record.NextAttemptAt, "last_error_code": record.LastErrorCode})
		if result.Error != nil {
			return coordinatorStorageError("record landing failure in terminal commit", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent changed during failure commit"))
		}
		updated = landingIntentFrom(record)
		if record.State == domain.LandingBlocked {
			if err := persistDesiredCheckTx(tx, input.BlockedCheck); err != nil {
				return err
			}
			return completeClaimTx(tx, input.Claim, input.ResultPayload)
		}
		return failClaimTx(tx, input.Claim)
	})
	return updated, err
}

func validateDesiredCheck(desired DesiredCheck) error {
	if desired.LogicalKey == "" || desired.LogicalKey != DesiredCheckLogicalKey(desired.Change, desired.HeadSHA) || desired.HeadSHA == "" || desired.Summary == "" || desired.UpdatedAt.IsZero() {
		return domain.NewError(domain.CodeValidation, errors.New("desired check is incomplete"))
	}
	switch desired.State {
	case forge.CheckPlanning, forge.CheckReady, forge.CheckReviewRequired, forge.CheckBlocked, forge.CheckSuperseded:
		return nil
	default:
		return domain.NewError(domain.CodeValidation, errors.New("desired check state is invalid"))
	}
}

func upsertDesiredCheckTx(tx *gorm.DB, desired DesiredCheck) (DesiredCheck, bool, error) {
	var existing desiredCheckRecord
	err := tx.Where("logical_key = ?", desired.LogicalKey).Take(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		desired.Version = 1
		record := desiredCheckRecordFrom(desired)
		if err := tx.Create(&record).Error; err != nil {
			return DesiredCheck{}, false, coordinatorStorageError("create desired check", err)
		}
		return desired, true, nil
	}
	if err != nil {
		return DesiredCheck{}, false, coordinatorStorageError("read desired check", err)
	}
	current := desiredCheckFrom(existing)
	if current.Change != desired.Change || current.HeadSHA != desired.HeadSHA {
		return DesiredCheck{}, false, domain.NewError(domain.CodeValidation, errors.New("desired check logical key has different identity"))
	}
	if current.State == desired.State && current.Summary == desired.Summary && current.PlanURL == desired.PlanURL {
		return current, false, nil
	}
	desired.Version = current.Version + 1
	desired.PublishedVersion = current.PublishedVersion
	desired.ProviderCheckRunID = current.ProviderCheckRunID
	result := tx.Model(&desiredCheckRecord{}).Where("logical_key = ? AND version = ?", desired.LogicalKey, current.Version).Updates(map[string]any{"state": desired.State, "summary": desired.Summary, "plan_url": desired.PlanURL, "version": desired.Version, "updated_at": desired.UpdatedAt.UTC()})
	if result.Error != nil {
		return DesiredCheck{}, false, coordinatorStorageError("update desired check", result.Error)
	}
	if result.RowsAffected != 1 {
		return DesiredCheck{}, false, domain.NewError(domain.CodeConcurrentUpdate, errors.New("desired check changed concurrently"))
	}
	return desired, true, nil
}

func persistDesiredCheckTx(tx *gorm.DB, desired DesiredCheck) error {
	stored, changed, err := upsertDesiredCheckTx(tx, desired)
	if err != nil || !changed {
		return err
	}
	job, err := newCheckJob(stored, desired.UpdatedAt, 5)
	if err != nil {
		return err
	}
	_, err = enqueueJobTx(tx, job)
	return err
}

func persistExistingDesiredCheckTx(tx *gorm.DB, desired DesiredCheck, now time.Time) error {
	job, err := newCheckJob(desired, now, 5)
	if err != nil {
		return err
	}
	_, err = enqueueJobTx(tx, job)
	return err
}

func ensureCheckJobTx(tx *gorm.DB, desired DesiredCheck, now time.Time, rearmTerminal bool) (bool, error) {
	job, err := newCheckJob(desired, now, 5)
	if err != nil {
		return false, err
	}
	var existing coordinatorJobRecord
	err = tx.Where("dedupe_key = ?", job.DedupeKey).Take(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return enqueueJobTx(tx, job)
	}
	if err != nil {
		return false, coordinatorStorageError("read desired check publication job", err)
	}
	if existing.ID != job.ID || existing.Kind != job.Kind || !bytes.Equal(existing.Payload, job.Payload) {
		return false, domain.NewError(domain.CodeValidation, errors.New("desired check job dedupe key has different content"))
	}
	if !rearmTerminal || (existing.State != CoordinatorJobDone && existing.State != CoordinatorJobFailed && existing.State != CoordinatorJobIncompatible) {
		return false, nil
	}
	result := tx.Model(&coordinatorJobRecord{}).Where("job_id = ? AND state = ? AND fencing_token = ?", existing.ID, existing.State, existing.FencingToken).Updates(map[string]any{
		"state":           CoordinatorJobRetryable,
		"attempts":        0,
		"next_attempt_at": now.UTC(),
		"lease_owner":     "",
		"lease_until":     time.Time{},
		"failure_code":    "",
		"result":          []byte(nil),
	})
	if result.Error != nil {
		return false, coordinatorStorageError("rearm desired check publication job", result.Error)
	}
	return result.RowsAffected == 1, nil
}

func desiredCheckRecordFrom(desired DesiredCheck) desiredCheckRecord {
	return desiredCheckRecord{LogicalKey: desired.LogicalKey, Provider: desired.Change.Provider, ForgeRepository: desired.Change.Repository, ChangeNumber: desired.Change.Number, HeadSHA: strings.ToLower(desired.HeadSHA), State: desired.State, Summary: desired.Summary, PlanURL: desired.PlanURL, Version: desired.Version, PublishedVersion: desired.PublishedVersion, ProviderCheckRunID: desired.ProviderCheckRunID, UpdatedAt: desired.UpdatedAt.UTC()}
}

func desiredCheckFrom(record desiredCheckRecord) DesiredCheck {
	return DesiredCheck{LogicalKey: record.LogicalKey, Change: domain.ChangeKey{Provider: record.Provider, Repository: record.ForgeRepository, Number: record.ChangeNumber}, HeadSHA: record.HeadSHA, State: record.State, Summary: record.Summary, PlanURL: record.PlanURL, Version: record.Version, PublishedVersion: record.PublishedVersion, ProviderCheckRunID: record.ProviderCheckRunID, UpdatedAt: record.UpdatedAt}
}

func newCheckJob(desired DesiredCheck, now time.Time, maxAttempts int) (CoordinatorJob, error) {
	payload, err := MarshalDurablePayload(checkJobKind, checkJobPayload{LogicalKey: desired.LogicalKey, Version: desired.Version})
	if err != nil {
		return CoordinatorJob{}, err
	}
	dedupe := "check:" + desired.LogicalKey + ":" + strconv.Itoa(desired.Version)
	digest := blake3.Sum256([]byte(dedupe))
	return CoordinatorJob{ID: fmt.Sprintf("%x", digest[:]), DedupeKey: dedupe, Kind: checkJobKind, Priority: checkJobPriority, Payload: payload, State: CoordinatorJobPending, MaxAttempts: maxAttempts, NextAttemptAt: now.UTC()}, nil
}
