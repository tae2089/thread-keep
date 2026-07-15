package runtime

import (
	"context"
	"sync"
	"testing"
	"time"
)

type blockingProcessor struct {
	mu        sync.Mutex
	active    int
	maxActive int
	started   chan struct{}
	release   chan struct{}
}

type reconcilingProcessor struct {
	reconciled chan struct{}
	once       sync.Once
}

type cancellationProcessor struct {
	started   chan struct{}
	cancelled chan error
}

func TestConfigRejectsHAAndInvalidTimeoutOrdering(t *testing.T) {
	config := DefaultConfig()
	config.Mode = ModeHA
	if err := config.Validate(); err == nil {
		t.Fatal("Validate(ha) error = nil, want unsupported mode")
	}
	config = DefaultConfig()
	config.JobTimeout = config.ExecutorTimeout + config.FinalizeMargin
	if err := config.Validate(); err == nil {
		t.Fatal("Validate(equal executor budget) error = nil")
	}
	config = DefaultConfig()
	config.LeaseDuration = config.JobTimeout
	if err := config.Validate(); err == nil {
		t.Fatal("Validate(equal lease) error = nil")
	}
}

func TestCoordinatorBoundsPlanningConcurrencyWithoutLocalClaimQueue(t *testing.T) {
	processor := &blockingProcessor{started: make(chan struct{}, 8), release: make(chan struct{})}
	config := DefaultConfig()
	config.Workers = 2
	config.PollInterval = time.Millisecond
	config.ShutdownGrace = time.Second
	coordinator, err := New(config, processor)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- coordinator.Run(ctx, "coordinator-1") }()
	for index := 0; index < config.Workers; index++ {
		select {
		case <-processor.started:
		case <-time.After(time.Second):
			t.Fatalf("planning worker %d did not start", index)
		}
	}
	select {
	case <-processor.started:
		t.Fatal("coordinator started more planning work than configured workers")
	case <-time.After(50 * time.Millisecond):
	}
	processor.mu.Lock()
	maxActive := processor.maxActive
	processor.mu.Unlock()
	if maxActive != config.Workers {
		t.Fatalf("max planning concurrency = %d, want %d", maxActive, config.Workers)
	}
	cancel()
	close(processor.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator did not shut down within grace")
	}
}

func TestCoordinatorRunsOneBoundedReconcilerLoop(t *testing.T) {
	processor := &reconcilingProcessor{reconciled: make(chan struct{})}
	config := DefaultConfig()
	config.PollInterval = time.Millisecond
	config.ReconcileInterval = time.Nanosecond
	config.ShutdownGrace = time.Second
	coordinator, err := New(config, processor)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- coordinator.Run(ctx, "coordinator-1") }()
	select {
	case <-processor.reconciled:
	case <-time.After(time.Second):
		t.Fatal("reconciler did not run")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not stop reconciler within grace")
	}
}

func TestCoordinatorShutdownCancelsInFlightProcessor(t *testing.T) {
	processor := &cancellationProcessor{started: make(chan struct{}), cancelled: make(chan error, 1)}
	config := DefaultConfig()
	config.Workers = 1
	config.PollInterval = time.Millisecond
	config.ShutdownGrace = 200 * time.Millisecond
	coordinator, err := New(config, processor)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- coordinator.Run(ctx, "coordinator-1") }()
	select {
	case <-processor.started:
	case <-time.After(time.Second):
		t.Fatal("processor did not start")
	}
	cancel()
	select {
	case err := <-processor.cancelled:
		if err != context.Canceled {
			t.Fatalf("processor cancellation = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("processor did not observe coordinator cancellation")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not stop after cancelling processor")
	}
}

func (p *blockingProcessor) RunOneKinds(ctx context.Context, _ string, kinds []string, _, _ time.Duration) (bool, error) {
	planning := false
	for _, kind := range kinds {
		if kind == "preview_plan" || kind == "final_plan" {
			planning = true
		}
	}
	if !planning {
		return false, nil
	}
	p.mu.Lock()
	p.active++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
	p.mu.Unlock()
	p.started <- struct{}{}
	select {
	case <-ctx.Done():
	case <-p.release:
	}
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return true, nil
}

func (p *reconcilingProcessor) RunOneKinds(context.Context, string, []string, time.Duration, time.Duration) (bool, error) {
	return false, nil
}

func (p *reconcilingProcessor) Reconcile(context.Context) error {
	p.once.Do(func() { close(p.reconciled) })
	return nil
}

func (p *cancellationProcessor) RunOneKinds(ctx context.Context, _ string, kinds []string, _, _ time.Duration) (bool, error) {
	for _, kind := range kinds {
		if kind == "preview_plan" || kind == "final_plan" {
			close(p.started)
			<-ctx.Done()
			p.cancelled <- ctx.Err()
			return true, ctx.Err()
		}
	}
	return false, nil
}
