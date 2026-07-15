package backend

import (
	"context"
	"errors"
	"sync"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
)

type LocalBackend struct {
	name    BackendName
	runner  planner.SourceRunner
	mu      sync.Mutex
	results map[string]Observation
}

func NewLocalBackend(name BackendName, runner planner.SourceRunner) (*LocalBackend, error) {
	if (name != BackendProcess && name != BackendInProcess) || runner == nil {
		return nil, domain.NewError(domain.CodeValidation, errors.New("local runner backend configuration is invalid"))
	}
	return &LocalBackend{name: name, runner: runner, results: make(map[string]Observation)}, nil
}

func (b *LocalBackend) Name() BackendName { return b.name }

func (b *LocalBackend) Adoptable() bool { return false }

func (b *LocalBackend) Ensure(ctx context.Context, spec ExecutionSpec) (BackendHandle, error) {
	handle := BackendHandle{Version: 1, Backend: b.name, ResourceID: spec.AttemptID, ExecutionID: spec.ExecutionID, AttemptID: spec.AttemptID, RequestDigest: spec.RequestDigest, SpecDigest: spec.SpecDigest}
	b.mu.Lock()
	_, exists := b.results[spec.AttemptID]
	b.mu.Unlock()
	if exists {
		return handle, nil
	}
	evidence, err := b.runner.IndexSource(ctx, spec.Request)
	observation := Observation{State: ObservationSucceeded}
	if err != nil {
		observation = Observation{State: ObservationFailed, FailureCode: domain.CodeOf(err)}
	} else {
		result, encodeErr := EncodeResult(ResultEnvelope{Version: resultEnvelopeVersion, Evidence: evidence})
		if encodeErr != nil {
			return BackendHandle{}, encodeErr
		}
		observation.ResultEnvelope = result
	}
	b.mu.Lock()
	b.results[spec.AttemptID] = observation
	b.mu.Unlock()
	return handle, nil
}

func (b *LocalBackend) Observe(_ context.Context, handle BackendHandle) (Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	observation, ok := b.results[handle.ResourceID]
	if !ok {
		return Observation{State: ObservationLost, FailureCode: domain.CodeCoverageIncomplete}, nil
	}
	return observation, nil
}

func (b *LocalBackend) Cancel(_ context.Context, handle BackendHandle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.results[handle.ResourceID] = Observation{State: ObservationFailed, FailureCode: domain.CodeBusy}
	return nil
}

func (b *LocalBackend) Cleanup(_ context.Context, handle BackendHandle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.results, handle.ResourceID)
	return nil
}

func (b *LocalBackend) CleanupDiscovered(_ context.Context, identity CleanupIdentity) error {
	if err := validateCleanupIdentity(identity); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.results, identity.AttemptID)
	return nil
}
