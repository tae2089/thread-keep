package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/tae2089/thread-keep/internal/domain"
)

type Membership interface {
	PeerProvider
	Start(ctx context.Context) error
	Leave(ctx context.Context) error
}

type dbMembership struct {
	provider *DBLeaseProvider
	interval time.Duration
}

type SwimOptions struct {
	Self     Peer
	BindAddr string
	Seeds    []string
	Secret   string
}

type SwimMembership struct {
	options SwimOptions
	list    *memberlist.Memberlist
}

type swimDelegate struct {
	meta []byte
}

var (
	_ Membership = (*dbMembership)(nil)
	_ Membership = (*SwimMembership)(nil)
)

func newDBMembership(provider *DBLeaseProvider, interval time.Duration) *dbMembership {
	return &dbMembership{provider: provider, interval: interval}
}

func (m *dbMembership) Self() Peer { return m.provider.Self() }

func (m *dbMembership) Peers(ctx context.Context) ([]Peer, error) {
	return m.provider.Peers(ctx)
}

func (m *dbMembership) Start(ctx context.Context) error {
	if err := m.provider.Heartbeat(ctx); err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = m.provider.Heartbeat(ctx)
			}
		}
	}()
	return nil
}

func (m *dbMembership) Leave(ctx context.Context) error {
	return m.provider.Leave(ctx)
}

func NewSwimMembership(options SwimOptions) (*SwimMembership, error) {
	if strings.TrimSpace(options.Self.NodeID) == "" || strings.TrimSpace(options.Self.BaseURL) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("swim membership requires a node id and advertise URL"))
	}
	if strings.TrimSpace(options.Secret) == "" {
		return nil, domain.NewError(domain.CodeValidation, errors.New("swim membership requires the cluster secret"))
	}
	if _, _, err := net.SplitHostPort(options.BindAddr); err != nil {
		return nil, domain.NewError(domain.CodeValidation, fmt.Errorf("swim bind address must be host:port: %w", err))
	}
	return &SwimMembership{options: options}, nil
}

func (m *SwimMembership) Self() Peer { return m.options.Self }

func (m *SwimMembership) Start(_ context.Context) error {
	host, portString, err := net.SplitHostPort(m.options.BindAddr)
	if err != nil {
		return domain.NewError(domain.CodeValidation, fmt.Errorf("swim bind address must be host:port: %w", err))
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		return domain.NewError(domain.CodeValidation, fmt.Errorf("swim bind port is invalid: %w", err))
	}
	config := memberlist.DefaultLANConfig()
	config.Name = m.options.Self.NodeID
	config.BindAddr = host
	config.BindPort = port
	config.AdvertisePort = port
	digest := sha256.Sum256([]byte(m.options.Secret))
	config.SecretKey = digest[:]
	config.Delegate = &swimDelegate{meta: []byte(m.options.Self.BaseURL)}
	config.LogOutput = io.Discard
	list, err := memberlist.Create(config)
	if err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("start swim membership: %w", err))
	}
	if len(m.options.Seeds) > 0 {
		if _, err := list.Join(m.options.Seeds); err != nil {
			_ = list.Shutdown()
			return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("join swim cluster: %w", err))
		}
	}
	m.list = list
	return nil
}

func (m *SwimMembership) Leave(_ context.Context) error {
	if m.list == nil {
		return nil
	}
	list := m.list
	m.list = nil
	if err := list.Leave(2 * time.Second); err != nil {
		_ = list.Shutdown()
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("leave swim cluster: %w", err))
	}
	if err := list.Shutdown(); err != nil {
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("stop swim membership: %w", err))
	}
	return nil
}

func (m *SwimMembership) Peers(_ context.Context) ([]Peer, error) {
	if m.list == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("swim membership is not started"))
	}
	members := m.list.Members()
	peers := make([]Peer, 0, len(members))
	for _, member := range members {
		if member.Name == m.options.Self.NodeID || len(member.Meta) == 0 {
			continue
		}
		peers = append(peers, Peer{NodeID: member.Name, BaseURL: string(member.Meta)})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].NodeID < peers[j].NodeID })
	return peers, nil
}

func (d *swimDelegate) NodeMeta(limit int) []byte {
	if len(d.meta) > limit {
		return d.meta[:limit]
	}
	return d.meta
}

func (d *swimDelegate) NotifyMsg([]byte) {}

func (d *swimDelegate) GetBroadcasts(int, int) [][]byte { return nil }

func (d *swimDelegate) LocalState(bool) []byte { return nil }

func (d *swimDelegate) MergeRemoteState([]byte, bool) {}
