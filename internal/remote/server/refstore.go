package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type GormRefStore struct {
	db       *gorm.DB
	postgres bool
}

type contextRefRecord struct {
	RepositoryID string `gorm:"primaryKey;column:repository_id"`
	RefName      string `gorm:"primaryKey;column:ref_name"`
	CommitID     string `gorm:"column:commit_id"`
	SourceSHA    string `gorm:"column:source_sha"`
	Version      int    `gorm:"column:version"`
}

type landingReceiptRecord struct {
	ReceiptID       string    `gorm:"primaryKey;column:receipt_id"`
	RepositoryID    string    `gorm:"index;column:repository_id"`
	RefName         string    `gorm:"column:ref_name"`
	ContextCommitID string    `gorm:"column:context_commit_id"`
	CreatedAt       time.Time `gorm:"column:created_at"`
}

func (contextRefRecord) TableName() string     { return "context_refs" }
func (landingReceiptRecord) TableName() string { return "landing_receipts" }

func OpenGormRefStore(dsn string) (*GormRefStore, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("ref database DSN must not be empty"))
	}
	usesPostgres := isPostgresDSN(dsn)
	if !usesPostgres && !strings.Contains(dsn, "?") {
		dsn += "?_busy_timeout=5000"
	}
	db, err := gorm.Open(selectDialector(dsn), &gorm.Config{Logger: logger.Discard, TranslateError: true})
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("open ref database: %w", err))
	}
	connection, err := db.DB()
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("configure ref database: %w", err))
	}
	if !usesPostgres {
		connection.SetMaxOpenConns(1)
	}
	models := append([]any{&contextRefRecord{}}, coordinatorModels()...)
	if err := db.AutoMigrate(models...); err != nil {
		_ = connection.Close()
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("migrate ref database: %w", err))
	}
	return &GormRefStore{db: db, postgres: usesPostgres}, nil
}

func (g *GormRefStore) Close() error {
	connection, err := g.db.DB()
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("close ref database: %w", err))
	}
	return connection.Close()
}

func (g *GormRefStore) ReadRef(ctx context.Context, repositoryID, refName string) (remote.Ref, error) {
	if refName == "" {
		return remote.Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote ref name must not be empty"))
	}
	var record contextRefRecord
	err := g.db.WithContext(ctx).Where("repository_id = ? AND ref_name = ?", repositoryID, refName).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return remote.Ref{RefName: refName}, nil
	}
	if err != nil {
		return remote.Ref{}, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("read remote ref: %w", err))
	}
	return remote.Ref{RefName: refName, CommitID: record.CommitID, SourceSHA: record.SourceSHA, Version: record.Version}, nil
}

func (g *GormRefStore) ListRefs(ctx context.Context, repositoryID string) ([]remote.Ref, error) {
	var records []contextRefRecord
	err := g.db.WithContext(ctx).Where("repository_id = ?", repositoryID).Order("ref_name").Find(&records).Error
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("list remote refs: %w", err))
	}
	refs := make([]remote.Ref, 0, len(records))
	for _, record := range records {
		refs = append(refs, remote.Ref{RefName: record.RefName, CommitID: record.CommitID, SourceSHA: record.SourceSHA, Version: record.Version})
	}
	return refs, nil
}

func (g *GormRefStore) CompareAndSwapRef(ctx context.Context, repositoryID, refName string, expected, next remote.Ref) (remote.Ref, error) {
	if expected.RefName != refName || next.RefName != refName || next.CommitID == "" || next.SourceSHA == "" || next.Version < 1 {
		return remote.Ref{}, domain.NewError(domain.CodeValidation, errors.New("remote ref compare-and-swap input is invalid"))
	}
	err := g.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record contextRefRecord
		findErr := tx.Where("repository_id = ? AND ref_name = ?", repositoryID, refName).Take(&record).Error
		current := remote.Ref{RefName: refName}
		exists := findErr == nil
		if exists {
			current = remote.Ref{RefName: refName, CommitID: record.CommitID, SourceSHA: record.SourceSHA, Version: record.Version}
		} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("read remote ref: %w", findErr))
		}
		if current != expected {
			return domain.NewError(domain.CodeRemoteConflict, errors.New("remote ref changed before compare-and-swap"))
		}
		if next.Version != current.Version+1 {
			return domain.NewError(domain.CodeValidation, errors.New("remote ref version must advance by one"))
		}
		if !exists {
			createErr := tx.Create(&contextRefRecord{RepositoryID: repositoryID, RefName: refName, CommitID: next.CommitID, SourceSHA: next.SourceSHA, Version: next.Version}).Error
			if errors.Is(createErr, gorm.ErrDuplicatedKey) {
				return domain.NewError(domain.CodeRemoteConflict, errors.New("remote ref changed before compare-and-swap"))
			}
			if createErr != nil {
				return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("create remote ref: %w", createErr))
			}
			return nil
		}
		result := tx.Model(&contextRefRecord{}).
			Where("repository_id = ? AND ref_name = ? AND version = ?", repositoryID, refName, expected.Version).
			Updates(map[string]any{"commit_id": next.CommitID, "source_sha": next.SourceSHA, "version": next.Version})
		if result.Error != nil {
			return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("update remote ref: %w", result.Error))
		}
		if result.RowsAffected != 1 {
			return domain.NewError(domain.CodeRemoteConflict, errors.New("remote ref changed before compare-and-swap"))
		}
		return nil
	})
	if err != nil {
		return remote.Ref{}, err
	}
	return next, nil
}

func selectDialector(dsn string) gorm.Dialector {
	if isPostgresDSN(dsn) {
		return postgres.Open(dsn)
	}
	return sqlite.Open(dsn)
}

func isPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") || strings.Contains(dsn, "host=")
}
