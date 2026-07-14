package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Peer struct {
	NodeID  string
	BaseURL string
}

type PeerProvider interface {
	Self() Peer
	Peers(ctx context.Context) ([]Peer, error)
}

type DBLeaseProvider struct {
	store *GormRefStore
	self  Peer
	ttl   time.Duration
}

type clusterNodeRecord struct {
	NodeID   string    `gorm:"primaryKey;column:node_id"`
	BaseURL  string    `gorm:"column:base_url"`
	LastSeen time.Time `gorm:"column:last_seen"`
}

var _ PeerProvider = (*DBLeaseProvider)(nil)

func (clusterNodeRecord) TableName() string { return "cluster_nodes" }

func NewDBLeaseProvider(store *GormRefStore, self Peer, ttl time.Duration) (*DBLeaseProvider, error) {
	if strings.TrimSpace(self.NodeID) == "" || strings.TrimSpace(self.BaseURL) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("cluster node id and advertise URL must not be empty"))
	}
	if ttl <= 0 {
		return nil, domain.NewError(domain.CodeValidation, errors.New("cluster lease TTL must be positive"))
	}
	if err := store.db.AutoMigrate(&clusterNodeRecord{}); err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("migrate cluster membership: %w", err))
	}
	return &DBLeaseProvider{store: store, self: self, ttl: ttl}, nil
}

func (p *DBLeaseProvider) Self() Peer { return p.self }

func (p *DBLeaseProvider) Heartbeat(ctx context.Context) error {
	err := p.store.db.WithContext(ctx).Model(&clusterNodeRecord{}).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "node_id"}},
		DoUpdates: clause.Assignments(map[string]any{"base_url": p.self.BaseURL, "last_seen": gorm.Expr("CURRENT_TIMESTAMP")}),
	}).Create(map[string]any{"node_id": p.self.NodeID, "base_url": p.self.BaseURL, "last_seen": gorm.Expr("CURRENT_TIMESTAMP")}).Error
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("record cluster heartbeat: %w", err))
	}
	return nil
}

func (p *DBLeaseProvider) Leave(ctx context.Context) error {
	err := p.store.db.WithContext(ctx).Where("node_id = ?", p.self.NodeID).Delete(&clusterNodeRecord{}).Error
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("remove cluster membership: %w", err))
	}
	return nil
}

func (p *DBLeaseProvider) Peers(ctx context.Context) ([]Peer, error) {
	freshness := "last_seen > datetime('now', ?)"
	argument := any(fmt.Sprintf("-%d seconds", int(p.ttl.Seconds())))
	if p.store.postgres {
		freshness = "last_seen > now() - make_interval(secs => ?)"
		argument = p.ttl.Seconds()
	}
	var records []clusterNodeRecord
	err := p.store.db.WithContext(ctx).Select("node_id", "base_url").
		Where(freshness, argument).
		Where("node_id <> ?", p.self.NodeID).
		Order("node_id").
		Find(&records).Error
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("list cluster peers: %w", err))
	}
	peers := make([]Peer, 0, len(records))
	for _, record := range records {
		peers = append(peers, Peer{NodeID: record.NodeID, BaseURL: record.BaseURL})
	}
	return peers, nil
}
