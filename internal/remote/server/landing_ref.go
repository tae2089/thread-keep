package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
	contextstore "github.com/tae2089/thread-keep/internal/store"
	"github.com/zeebo/blake3"
	"gorm.io/gorm"
)

type RefAdvanceInput struct {
	Expected remote.Ref
	Next     remote.Ref
	Object   domain.ContextObject
}

type LandingRefService struct {
	refs *GormRefStore
}

func NewLandingRefService(refs *GormRefStore) *LandingRefService {
	return &LandingRefService{refs: refs}
}

func (s *LandingRefService) Read(ctx context.Context, repositoryID, refName string) (remote.Ref, error) {
	return s.refs.ReadRef(ctx, repositoryID, refName)
}

func (s *LandingRefService) Advance(ctx context.Context, input RefAdvanceInput) (remote.Ref, error) {
	return s.advance(ctx, input, nil)
}

func (s *LandingRefService) AdvanceWithCheck(ctx context.Context, input RefAdvanceInput, desired DesiredCheck) (remote.Ref, error) {
	if err := validateDesiredCheck(desired); err != nil {
		return remote.Ref{}, err
	}
	return s.advance(ctx, input, &desired)
}

func (s *LandingRefService) advance(ctx context.Context, input RefAdvanceInput, desired *DesiredCheck) (remote.Ref, error) {
	receipt, err := validateLandingAdvance(input)
	if err != nil {
		return remote.Ref{}, err
	}
	confirmed := input.Next
	err = s.refs.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var indexed landingReceiptRecord
		if err := tx.Where("receipt_id = ?", receipt.ID).Take(&indexed).Error; err == nil {
			if indexed.RepositoryID != input.Object.RepositoryID || indexed.RefName != input.Object.RefName || indexed.ContextCommitID != input.Next.CommitID {
				return domain.NewError(domain.CodeValidation, errors.New("landing receipt ID is indexed with different content"))
			}
			var current contextRefRecord
			if err := tx.Where("repository_id = ? AND ref_name = ?", input.Object.RepositoryID, input.Object.RefName).Take(&current).Error; err != nil {
				return coordinatorStorageError("read idempotent landing ref", err)
			}
			confirmed = remote.Ref{RefName: current.RefName, CommitID: current.CommitID, SourceSHA: current.SourceSHA, Version: current.Version}
			if err := convergeLandedIntent(tx, receipt.ID, indexed.ContextCommitID); err != nil {
				return err
			}
			if desired != nil {
				return persistDesiredCheckTx(tx, *desired)
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return coordinatorStorageError("read landing receipt index", err)
		}
		var currentRecord contextRefRecord
		findErr := tx.Where("repository_id = ? AND ref_name = ?", input.Object.RepositoryID, input.Object.RefName).Take(&currentRecord).Error
		current := remote.Ref{RefName: input.Object.RefName}
		if findErr == nil {
			current = remote.Ref{RefName: currentRecord.RefName, CommitID: currentRecord.CommitID, SourceSHA: currentRecord.SourceSHA, Version: currentRecord.Version}
		} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return coordinatorStorageError("read landing ref", findErr)
		}
		if current != input.Expected {
			return domain.NewError(domain.CodeRemoteConflict, errors.New("context ref changed before landing transaction"))
		}
		if input.Next.Version != current.Version+1 {
			return domain.NewError(domain.CodeValidation, errors.New("landing ref version must advance by one"))
		}
		var intent landingIntentRecord
		if err := tx.Where("landing_id = ?", receipt.ID).Take(&intent).Error; err != nil {
			return coordinatorStorageError("read landing intent", err)
		}
		if err := validateReceiptIntent(receipt, intent, input.Object); err != nil {
			return err
		}
		result := tx.Model(&contextRefRecord{}).Where("repository_id = ? AND ref_name = ? AND version = ? AND commit_id = ?", input.Object.RepositoryID, input.Object.RefName, current.Version, current.CommitID).Updates(map[string]any{"commit_id": input.Next.CommitID, "source_sha": input.Next.SourceSHA, "version": input.Next.Version})
		if result.Error != nil {
			return coordinatorStorageError("advance landing ref", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeRemoteConflict, errors.New("context ref changed during landing transaction"))
		}
		if err := tx.Create(&landingReceiptRecord{ReceiptID: receipt.ID, RepositoryID: input.Object.RepositoryID, RefName: input.Object.RefName, ContextCommitID: input.Next.CommitID, CreatedAt: time.Now().UTC()}).Error; err != nil {
			return coordinatorStorageError("index landing receipt", err)
		}
		result = tx.Model(&landingIntentRecord{}).Where("landing_id = ? AND state IN ?", receipt.ID, []domain.LandingState{domain.LandingRunning, domain.LandingRecovering, domain.LandingPending}).Updates(map[string]any{"state": domain.LandingLanded, "final_plan_id": receipt.FinalPlanID, "landed_context_commit_id": input.Next.CommitID, "next_attempt_at": time.Time{}, "last_error_code": ""})
		if result.Error != nil {
			return coordinatorStorageError("complete landing intent", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent changed before transaction completion"))
		}
		if desired != nil {
			return persistDesiredCheckTx(tx, *desired)
		}
		return nil
	})
	if err != nil {
		return remote.Ref{}, err
	}
	return confirmed, nil
}

func validateLandingAdvance(input RefAdvanceInput) (domain.LandingReceipt, error) {
	if input.Next.RefName == "" || input.Next.RefName != input.Expected.RefName || input.Object.SchemaVersion != 4 || input.Object.RepositoryID == "" || input.Object.RefName != input.Next.RefName || input.Object.SourceSHA != input.Next.SourceSHA || len(input.Object.LandingReceipts) != 1 {
		return domain.LandingReceipt{}, domain.NewError(domain.CodeValidation, errors.New("landing ref advance is incomplete or mismatched"))
	}
	if err := contextstore.ValidateContextObject(input.Object, input.Object.RepositoryID, input.Object.RefName); err != nil {
		return domain.LandingReceipt{}, err
	}
	contents, err := json.Marshal(input.Object)
	if err != nil {
		return domain.LandingReceipt{}, domain.NewError(domain.CodeValidation, errors.New("serialize landing object"))
	}
	digest := blake3.Sum256(contents)
	if fmt.Sprintf("%x", digest[:]) != input.Next.CommitID {
		return domain.LandingReceipt{}, domain.NewError(domain.CodeValidation, errors.New("landing object ID does not match its canonical content"))
	}
	receipt := input.Object.LandingReceipts[0]
	if receipt.ID == "" || receipt.ContextRepositoryID != input.Object.RepositoryID || receipt.TargetRef != input.Object.RefName || receipt.SourceMergeSHA != input.Object.SourceSHA || receipt.FinalPlanID == "" || receipt.BaseContextCommitID != input.Expected.CommitID {
		return domain.LandingReceipt{}, domain.NewError(domain.CodeValidation, errors.New("landing receipt does not match its object or expected ref"))
	}
	return receipt, nil
}

func validateReceiptIntent(receipt domain.LandingReceipt, record landingIntentRecord, object domain.ContextObject) error {
	if record.RepositoryID != object.RepositoryID || record.TargetRef != object.RefName || record.Provider != receipt.Provider || record.ForgeRepository != receipt.ForgeRepository || record.ChangeNumber != receipt.ChangeNumber || record.CandidateDigest != receipt.CandidateDigest || record.SourceMergeSHA != receipt.SourceMergeSHA || record.FinalPlanID != receipt.FinalPlanID {
		return domain.NewError(domain.CodeValidation, errors.New("landing receipt does not match its durable intent"))
	}
	if record.State != domain.LandingRunning && record.State != domain.LandingRecovering && record.State != domain.LandingPending {
		return domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing intent is not in an advanceable state"))
	}
	return nil
}

func convergeLandedIntent(tx *gorm.DB, intentID, commitID string) error {
	result := tx.Model(&landingIntentRecord{}).Where("landing_id = ? AND state <> ?", intentID, domain.LandingLanded).Updates(map[string]any{"state": domain.LandingLanded, "landed_context_commit_id": commitID, "next_attempt_at": time.Time{}, "last_error_code": ""})
	if result.Error != nil {
		return coordinatorStorageError("converge idempotent landing intent", result.Error)
	}
	return nil
}
