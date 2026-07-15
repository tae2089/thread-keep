package runtime

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	ModeDurableSingle = "durable_single"
	ModeHA            = "ha"
)

var (
	controlKinds  = []string{"process_webhook", "publish_check"}
	planningKinds = []string{"final_plan", "preview_plan"}
)

type Config struct {
	Mode              string
	Replicas          int
	Workers           int
	ControlWorkers    int
	PollInterval      time.Duration
	ExecutorTimeout   time.Duration
	FinalizeMargin    time.Duration
	JobTimeout        time.Duration
	LeaseDuration     time.Duration
	ShutdownGrace     time.Duration
	CleanupTimeout    time.Duration
	HeartbeatInterval time.Duration
	ReconcileInterval time.Duration
	OnError           func(error)
}

type Processor interface {
	RunOneKinds(ctx context.Context, workerID string, kinds []string, jobTimeout, leaseDuration time.Duration) (bool, error)
}

type Reconciler interface {
	Reconcile(ctx context.Context) error
}

type CoordinatorIdentity struct {
	InstanceID  string
	DisplayName string
}

type CoordinatorLease struct {
	Slot       string
	InstanceID string
	Token      string
}

type HeartbeatStore interface {
	AcquireCoordinator(ctx context.Context, identity CoordinatorIdentity, ttl time.Duration) (CoordinatorLease, error)
	RefreshCoordinator(ctx context.Context, lease CoordinatorLease, ttl time.Duration) error
	ReleaseCoordinator(ctx context.Context, lease CoordinatorLease) error
}

type Coordinator struct {
	config     Config
	processor  Processor
	heartbeats HeartbeatStore
}

func DefaultConfig() Config {
	return Config{Mode: ModeDurableSingle, Replicas: 1, Workers: 2, ControlWorkers: 1, PollInterval: time.Second, ExecutorTimeout: 2 * time.Minute, FinalizeMargin: 30 * time.Second, JobTimeout: 3 * time.Minute, LeaseDuration: 4 * time.Minute, ShutdownGrace: 30 * time.Second, CleanupTimeout: 10 * time.Second, HeartbeatInterval: 15 * time.Second, ReconcileInterval: 30 * time.Second}
}

func (c Config) Validate() error {
	if c.Mode == ModeHA {
		return errors.New("coordinator HA mode requires renewal and fencing support")
	}
	if c.Mode != ModeDurableSingle || c.Replicas != 1 {
		return errors.New("coordinator durable_single mode requires exactly one declared replica")
	}
	if c.Workers < 1 || c.Workers > 32 || c.ControlWorkers != 1 {
		return errors.New("coordinator requires 1..32 planning workers and one control worker")
	}
	if c.PollInterval <= 0 || c.ExecutorTimeout <= 0 || c.FinalizeMargin <= 0 || c.JobTimeout <= 0 || c.LeaseDuration <= 0 || c.ShutdownGrace <= 0 || c.CleanupTimeout <= 0 || c.HeartbeatInterval <= 0 || c.ReconcileInterval <= 0 {
		return errors.New("coordinator durations must be positive")
	}
	if c.ExecutorTimeout+c.FinalizeMargin >= c.JobTimeout || c.JobTimeout >= c.LeaseDuration {
		return errors.New("coordinator timeouts must satisfy executor_timeout + finalize_margin < job_timeout < lease_duration")
	}
	return nil
}

func New(config Config, processor Processor) (*Coordinator, error) {
	return newCoordinator(config, processor, nil)
}

func NewGuarded(config Config, processor Processor, heartbeats HeartbeatStore) (*Coordinator, error) {
	if heartbeats == nil {
		return nil, errors.New("coordinator heartbeat store is required")
	}
	return newCoordinator(config, processor, heartbeats)
}

func newCoordinator(config Config, processor Processor, heartbeats HeartbeatStore) (*Coordinator, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if processor == nil {
		return nil, errors.New("coordinator processor is required")
	}
	return &Coordinator{config: config, processor: processor, heartbeats: heartbeats}, nil
}

func (c *Coordinator) Run(ctx context.Context, coordinatorID string) error {
	coordinatorID = strings.TrimSpace(coordinatorID)
	if coordinatorID == "" {
		return errors.New("coordinator display name is required")
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	heartbeatDone := make(chan struct{})
	heartbeatErrors := make(chan error, 1)
	workerPrefix := coordinatorID
	if c.heartbeats != nil {
		identity := CoordinatorIdentity{InstanceID: rand.Text(), DisplayName: coordinatorID}
		lease, err := c.heartbeats.AcquireCoordinator(runCtx, identity, 2*c.config.HeartbeatInterval)
		if err != nil {
			return err
		}
		workerPrefix = lease.InstanceID
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), c.config.CleanupTimeout)
			defer cancel()
			_ = c.heartbeats.ReleaseCoordinator(cleanupCtx, lease)
		}()
		go c.runHeartbeat(runCtx, cancelRun, lease, heartbeatErrors, heartbeatDone)
	} else {
		close(heartbeatDone)
	}
	var workers sync.WaitGroup
	for index := 0; index < c.config.ControlWorkers; index++ {
		workers.Add(1)
		go c.runWorker(runCtx, &workers, fmt.Sprintf("%s-control-%d", workerPrefix, index+1), controlKinds)
	}
	for index := 0; index < c.config.Workers; index++ {
		workers.Add(1)
		go c.runWorker(runCtx, &workers, fmt.Sprintf("%s-planning-%d", workerPrefix, index+1), planningKinds)
	}
	if reconciler, ok := c.processor.(Reconciler); ok {
		workers.Add(1)
		go c.runReconciler(runCtx, &workers, reconciler)
	}
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		cancelRun()
		<-heartbeatDone
		select {
		case err := <-heartbeatErrors:
			return err
		default:
			return nil
		}
	case <-ctx.Done():
		cancelRun()
	}
	timer := time.NewTimer(c.config.ShutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		<-heartbeatDone
		return nil
	case <-timer.C:
		return errors.New("coordinator shutdown grace expired")
	}
}

func (c *Coordinator) runReconciler(ctx context.Context, workers *sync.WaitGroup, reconciler Reconciler) {
	defer workers.Done()
	ticker := time.NewTicker(c.config.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := reconciler.Reconcile(ctx); err != nil && c.config.OnError != nil {
				c.config.OnError(err)
			}
		}
	}
}

func (c *Coordinator) runHeartbeat(ctx context.Context, cancel context.CancelFunc, lease CoordinatorLease, errorsOut chan<- error, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.heartbeats.RefreshCoordinator(ctx, lease, 2*c.config.HeartbeatInterval); err != nil {
				if c.config.OnError != nil {
					c.config.OnError(err)
				}
				errorsOut <- err
				cancel()
				return
			}
		}
	}
}

func (c *Coordinator) runWorker(ctx context.Context, workers *sync.WaitGroup, workerID string, kinds []string) {
	defer workers.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, c.config.JobTimeout)
		processed, err := c.processor.RunOneKinds(jobCtx, workerID, kinds, c.config.JobTimeout, c.config.LeaseDuration)
		cancel()
		if err != nil && c.config.OnError != nil {
			c.config.OnError(err)
		}
		if ctx.Err() != nil {
			return
		}
		if processed && err == nil {
			continue
		}
		timer := time.NewTimer(c.config.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}
