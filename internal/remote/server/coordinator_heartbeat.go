package server

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/coordinator/runtime"
	"github.com/tae2089/thread-keep/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type coordinatorHeartbeatRecord struct {
	Slot         string    `gorm:"primaryKey;column:slot"`
	InstanceID   string    `gorm:"uniqueIndex;column:instance_id"`
	DisplayName  string    `gorm:"column:display_name"`
	SessionToken string    `gorm:"column:session_token"`
	Mode         string    `gorm:"column:mode"`
	ExpiresAt    time.Time `gorm:"index;column:expires_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

type CoordinatorIdentity = runtime.CoordinatorIdentity

type CoordinatorLease = runtime.CoordinatorLease

func (coordinatorHeartbeatRecord) TableName() string { return "runner_heartbeats" }

func (g *GormRefStore) AcquireCoordinator(ctx context.Context, identity CoordinatorIdentity, ttl time.Duration) (CoordinatorLease, error) {
	if strings.TrimSpace(identity.InstanceID) == "" || strings.TrimSpace(identity.DisplayName) == "" || ttl <= 0 {
		return CoordinatorLease{}, domain.NewError(domain.CodeValidation, errors.New("coordinator heartbeat input is invalid"))
	}
	lease := CoordinatorLease{Slot: "durable_single", InstanceID: identity.InstanceID, Token: rand.Text()}
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		if err := tx.Where("expires_at <= ?", now).Delete(&coordinatorHeartbeatRecord{}).Error; err != nil {
			return coordinatorStorageError("delete expired coordinator heartbeats", err)
		}
		query := tx.Where("slot = ?", "durable_single")
		if g.postgres {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var active coordinatorHeartbeatRecord
		if err := query.Take(&active).Error; err == nil {
			return domain.NewError(domain.CodeBusy, errors.New("another durable single coordinator heartbeat is active"))
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return coordinatorStorageError("read active coordinator heartbeat", err)
		}
		record := coordinatorHeartbeatRecord{Slot: lease.Slot, InstanceID: identity.InstanceID, DisplayName: identity.DisplayName, SessionToken: lease.Token, Mode: "durable_single", ExpiresAt: now.Add(ttl), UpdatedAt: now}
		if err := tx.Create(&record).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return domain.NewError(domain.CodeBusy, errors.New("another durable single coordinator acquired the heartbeat concurrently"))
			}
			return coordinatorStorageError("acquire coordinator heartbeat", err)
		}
		return nil
	})
	if err != nil {
		return CoordinatorLease{}, err
	}
	return lease, nil
}

func (g *GormRefStore) RefreshCoordinator(ctx context.Context, lease CoordinatorLease, ttl time.Duration) error {
	if strings.TrimSpace(lease.Slot) == "" || strings.TrimSpace(lease.InstanceID) == "" || strings.TrimSpace(lease.Token) == "" || ttl <= 0 {
		return domain.NewError(domain.CodeValidation, errors.New("coordinator heartbeat input is invalid"))
	}
	return g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now, err := databaseTime(tx)
		if err != nil {
			return err
		}
		result := tx.Model(&coordinatorHeartbeatRecord{}).Where("slot = ? AND instance_id = ? AND session_token = ? AND expires_at > ?", lease.Slot, lease.InstanceID, lease.Token, now).Updates(map[string]any{"expires_at": now.Add(ttl), "updated_at": now})
		if result.Error != nil {
			return coordinatorStorageError("refresh coordinator heartbeat", result.Error)
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeBusy, errors.New("coordinator heartbeat is no longer active"))
		}
		return nil
	})
}

func (g *GormRefStore) ReleaseCoordinator(ctx context.Context, lease CoordinatorLease) error {
	if strings.TrimSpace(lease.Slot) == "" || strings.TrimSpace(lease.InstanceID) == "" || strings.TrimSpace(lease.Token) == "" {
		return domain.NewError(domain.CodeValidation, errors.New("coordinator lease is incomplete"))
	}
	if err := g.db.WithContext(ctx).Where("slot = ? AND instance_id = ? AND session_token = ?", lease.Slot, lease.InstanceID, lease.Token).Delete(&coordinatorHeartbeatRecord{}).Error; err != nil {
		return coordinatorStorageError("release coordinator heartbeat", err)
	}
	return nil
}
