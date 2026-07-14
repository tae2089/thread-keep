package backend

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/tae2089/thread-keep/internal/remote/server"
	"github.com/tae2089/thread-keep/internal/runner/protocol"
	"github.com/zeebo/blake3"
)

type BackendName string

const (
	BackendProcess       BackendName = "process"
	BackendInProcess     BackendName = "in_process"
	BackendDocker        BackendName = "docker"
	BackendKubernetesJob BackendName = "kubernetes_job"

	resultEnvelopeVersion = protocol.ResultEnvelopeVersion
)

type ObservationState string

const (
	ObservationRunning   ObservationState = "running"
	ObservationSucceeded ObservationState = "succeeded"
	ObservationFailed    ObservationState = "failed"
	ObservationLost      ObservationState = "lost"
)

type ExecutionSpec struct {
	ExecutionID   string
	AttemptID     string
	RunnerAttempt int
	RequestDigest string
	SpecDigest    string
	Timeout       time.Duration
	Request       planner.SourceRequest
}

type BackendHandle struct {
	Version     int         `json:"version"`
	Backend     BackendName `json:"backend"`
	ResourceID  string      `json:"resource_id"`
	ExecutionID string      `json:"execution_id,omitempty"`
	AttemptID   string      `json:"attempt_id,omitempty"`
	SpecDigest  string      `json:"spec_digest,omitempty"`
}

type Observation struct {
	State          ObservationState
	ResultEnvelope []byte
	FailureCode    domain.ErrorCode
}

type ResultEnvelope = protocol.ResultEnvelope

type RunnerBackend interface {
	Name() BackendName
	Adoptable() bool
	Ensure(ctx context.Context, spec ExecutionSpec) (BackendHandle, error)
	Observe(ctx context.Context, handle BackendHandle) (Observation, error)
	Cancel(ctx context.Context, handle BackendHandle) error
	Cleanup(ctx context.Context, handle BackendHandle) error
}

type DurableSourceRunnerConfig struct {
	Store        *server.GormRefStore
	Backend      RunnerBackend
	InstanceID   string
	SpecDigest   string
	Timeout      time.Duration
	CleanupDelay time.Duration
	Wait         func(context.Context) error
}

type DurableSourceRunner struct {
	store        *server.GormRefStore
	backend      RunnerBackend
	instanceID   string
	specDigest   string
	timeout      time.Duration
	cleanupDelay time.Duration
	wait         func(context.Context) error
}

func NewDurableSourceRunner(config DurableSourceRunnerConfig) (*DurableSourceRunner, error) {
	if config.Store == nil || config.Backend == nil || strings.TrimSpace(config.InstanceID) == "" || !validDigest(config.SpecDigest) {
		return nil, domain.NewError(domain.CodeValidation, errors.New("durable runner configuration is incomplete"))
	}
	if config.Timeout <= 0 {
		config.Timeout = 2 * time.Minute
	}
	if config.CleanupDelay <= 0 {
		config.CleanupDelay = 10 * time.Minute
	}
	if config.Wait == nil {
		config.Wait = func(ctx context.Context) error {
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	return &DurableSourceRunner{store: config.Store, backend: config.Backend, instanceID: config.InstanceID, specDigest: config.SpecDigest, timeout: config.Timeout, cleanupDelay: config.CleanupDelay, wait: config.Wait}, nil
}

func (r *DurableSourceRunner) IndexClaimedSource(ctx context.Context, claim planner.RunnerClaim, request planner.SourceRequest) (planner.SourceEvidence, error) {
	storeClaim := server.JobClaim{JobID: claim.JobID, LeaseOwner: claim.LeaseOwner, FencingToken: claim.FencingToken}
	requestDigest, err := RequestDigest(request)
	if err != nil {
		return planner.SourceEvidence{}, err
	}
	executionID, err := ExecutionID(claim.JobID, requestDigest)
	if err != nil {
		return planner.SourceEvidence{}, err
	}
	firstAttemptID, err := AttemptID(executionID, 1)
	if err != nil {
		return planner.SourceEvidence{}, err
	}
	execution, err := r.store.PrepareRunnerExecution(ctx, storeClaim, server.RunnerExecutionSeed{ExecutionID: executionID, JobID: claim.JobID, RequestDigest: requestDigest, SpecDigest: r.specDigest, Backend: string(r.backend.Name()), AttemptID: firstAttemptID, OwnerInstance: r.instanceID})
	if err != nil {
		return planner.SourceEvidence{}, err
	}
	execution, err = r.ownAttempt(ctx, storeClaim, execution)
	if err != nil {
		return planner.SourceEvidence{}, err
	}
	return r.runAttempt(ctx, storeClaim, execution, request)
}

func (r *DurableSourceRunner) Reconcile(ctx context.Context) error {
	executions, err := r.store.ListRunnerExecutionsForReconciliation(ctx, 100)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, execution := range executions {
		if execution.CleanupState != server.RunnerCleanupPending && execution.CleanupState != server.RunnerCleanupFailed {
			continue
		}
		if !execution.CleanupAfter.IsZero() && execution.CleanupAfter.After(now) {
			continue
		}
		if execution.Backend != string(r.backend.Name()) {
			continue
		}
		if len(execution.HandleEnvelope) == 0 {
			if err := r.store.MarkRunnerCleaned(ctx, execution.ExecutionID, execution.AttemptID); err != nil {
				return err
			}
			continue
		}
		var handle BackendHandle
		if err := json.Unmarshal(execution.HandleEnvelope, &handle); err != nil {
			if recordErr := r.store.RecordRunnerCleanupFailure(ctx, execution.ExecutionID, execution.AttemptID); recordErr != nil {
				return recordErr
			}
			continue
		}
		if err := r.backend.Cleanup(ctx, handle); err != nil {
			if recordErr := r.store.RecordRunnerCleanupFailure(ctx, execution.ExecutionID, execution.AttemptID); recordErr != nil {
				return recordErr
			}
			continue
		}
		if err := r.store.MarkRunnerCleaned(ctx, execution.ExecutionID, execution.AttemptID); err != nil {
			return err
		}
	}
	return nil
}

func (r *DurableSourceRunner) ownAttempt(ctx context.Context, claim server.JobClaim, execution server.RunnerExecution) (server.RunnerExecution, error) {
	owned := execution.ClaimOwner == claim.LeaseOwner && execution.ClaimFencingToken == claim.FencingToken
	switch execution.State {
	case server.RunnerExecutionPrepared, server.RunnerExecutionActive, server.RunnerExecutionSucceeded:
		if owned {
			return execution, nil
		}
		if r.backend.Adoptable() {
			if execution.SpecDigest != r.specDigest {
				return server.RunnerExecution{}, domain.NewError(domain.CodeValidation, errors.New("active runner attempt spec changed"))
			}
			return r.store.AdoptRunnerExecution(ctx, claim, execution.ExecutionID, execution.AttemptID)
		}
		nextID, err := AttemptID(execution.ExecutionID, execution.RunnerAttempt+1)
		if err != nil {
			return server.RunnerExecution{}, err
		}
		return r.store.ReplaceLocalRunnerAttempt(ctx, claim, execution.ExecutionID, execution.AttemptID, nextID, r.specDigest, r.instanceID)
	case server.RunnerExecutionFailed, server.RunnerExecutionCancelled, server.RunnerExecutionLost:
		nextID, err := AttemptID(execution.ExecutionID, execution.RunnerAttempt+1)
		if err != nil {
			return server.RunnerExecution{}, err
		}
		return r.store.AllocateRunnerAttempt(ctx, claim, execution.ExecutionID, execution.AttemptID, nextID, r.specDigest, r.instanceID)
	default:
		return server.RunnerExecution{}, domain.NewError(domain.CodeConcurrentUpdate, errors.New("runner execution is not runnable"))
	}
}

func (r *DurableSourceRunner) runAttempt(ctx context.Context, claim server.JobClaim, execution server.RunnerExecution, request planner.SourceRequest) (planner.SourceEvidence, error) {
	handle := BackendHandle{}
	if len(execution.HandleEnvelope) == 0 {
		var err error
		handle, err = r.backend.Ensure(ctx, ExecutionSpec{ExecutionID: execution.ExecutionID, AttemptID: execution.AttemptID, RunnerAttempt: execution.RunnerAttempt, RequestDigest: execution.RequestDigest, SpecDigest: execution.SpecDigest, Timeout: r.timeout, Request: request})
		request.Credential = ""
		if err != nil {
			if handle.ResourceID != "" {
				encoded, encodeErr := json.Marshal(handle)
				if encodeErr != nil {
					return planner.SourceEvidence{}, domain.NewError(domain.CodeValidation, errors.New("serialize discovered runner handle"))
				}
				if recordErr := r.store.RecordDiscoveredRunnerHandle(context.Background(), execution.ExecutionID, execution.AttemptID, string(r.backend.Name()), execution.SpecDigest, encoded); recordErr != nil {
					return planner.SourceEvidence{}, recordErr
				}
			}
			r.recordFailure(claim, execution, domain.CodeOf(err))
			return planner.SourceEvidence{}, err
		}
		encoded, err := json.Marshal(handle)
		if err != nil {
			return planner.SourceEvidence{}, domain.NewError(domain.CodeValidation, errors.New("serialize runner handle"))
		}
		if err := r.store.RecordRunnerHandle(ctx, claim, execution.ExecutionID, execution.AttemptID, encoded); err != nil {
			_ = r.store.RecordDiscoveredRunnerHandle(context.Background(), execution.ExecutionID, execution.AttemptID, string(r.backend.Name()), execution.SpecDigest, encoded)
			return planner.SourceEvidence{}, err
		}
	} else if err := json.Unmarshal(execution.HandleEnvelope, &handle); err != nil {
		return planner.SourceEvidence{}, domain.NewError(domain.CodeIncompatiblePayload, errors.New("stored runner handle is invalid"))
	}
	for {
		observation, err := r.backend.Observe(ctx, handle)
		if err != nil {
			if ctx.Err() != nil {
				return planner.SourceEvidence{}, r.cancelCurrentAttempt(claim, execution, handle)
			}
			return planner.SourceEvidence{}, err
		}
		switch observation.State {
		case ObservationRunning:
			if err := r.wait(ctx); err != nil {
				return planner.SourceEvidence{}, r.cancelCurrentAttempt(claim, execution, handle)
			}
		case ObservationSucceeded:
			envelope, err := DecodeResult(observation.ResultEnvelope)
			if err != nil {
				r.recordFailure(claim, execution, domain.CodeOf(err))
				return planner.SourceEvidence{}, err
			}
			if envelope.Code != "" {
				err := domain.NewError(envelope.Code, errors.New(safeResultMessage(envelope.Code)))
				r.recordFailure(claim, execution, envelope.Code)
				return planner.SourceEvidence{}, err
			}
			if err := planner.ValidateSourceEvidence(request, envelope.Evidence); err != nil {
				r.recordFailure(claim, execution, domain.CodeOf(err))
				return planner.SourceEvidence{}, err
			}
			digest := blake3.Sum256(observation.ResultEnvelope)
			if err := r.store.CompleteRunnerExecution(ctx, claim, execution.ExecutionID, execution.AttemptID, hex.EncodeToString(digest[:]), time.Now().Add(r.cleanupDelay)); err != nil {
				return planner.SourceEvidence{}, err
			}
			return envelope.Evidence, nil
		case ObservationFailed, ObservationLost:
			code := observation.FailureCode
			if code == "" {
				code = domain.CodeCoverageIncomplete
			}
			r.recordFailure(claim, execution, code)
			return planner.SourceEvidence{}, domain.NewError(code, errors.New(safeResultMessage(code)))
		default:
			return planner.SourceEvidence{}, domain.NewError(domain.CodeIncompatiblePayload, errors.New("runner observation state is invalid"))
		}
	}
}

func (r *DurableSourceRunner) cancelCurrentAttempt(claim server.JobClaim, execution server.RunnerExecution, handle BackendHandle) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.store.BeginRunnerCancel(cleanupCtx, claim, execution.ExecutionID, execution.AttemptID); err != nil {
		return err
	}
	if err := r.backend.Cancel(cleanupCtx, handle); err != nil {
		_ = r.store.FailRunnerExecution(cleanupCtx, claim, execution.ExecutionID, execution.AttemptID, domain.CodeBusy)
		_ = r.store.MarkRunnerCleanupPending(cleanupCtx, execution.ExecutionID, execution.AttemptID)
		return err
	}
	if err := r.store.MarkRunnerCancelled(cleanupCtx, claim, execution.ExecutionID, execution.AttemptID); err != nil {
		return err
	}
	return domain.NewError(domain.CodeBusy, errors.New("runner execution timed out or was cancelled"))
}

func (r *DurableSourceRunner) recordFailure(claim server.JobClaim, execution server.RunnerExecution, code domain.ErrorCode) {
	if code == "" {
		code = domain.CodeCoverageIncomplete
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.store.FailRunnerExecution(cleanupCtx, claim, execution.ExecutionID, execution.AttemptID, code); err == nil {
		_ = r.store.MarkRunnerCleanupPending(cleanupCtx, execution.ExecutionID, execution.AttemptID)
	}
}

func EncodeResult(envelope ResultEnvelope) ([]byte, error) {
	return protocol.EncodeResult(envelope)
}

func DecodeResult(contents []byte) (ResultEnvelope, error) {
	return protocol.DecodeResult(contents)
}

func safeResultMessage(code domain.ErrorCode) string {
	switch code {
	case domain.CodeBusy:
		return "runner execution timed out or was cancelled"
	case domain.CodeAuth:
		return "runner execution is not authorized"
	case domain.CodeValidation:
		return "runner execution input is invalid"
	default:
		return "runner execution failed"
	}
}
