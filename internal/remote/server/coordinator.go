package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/forge"
	"github.com/tae2089/thread-keep/internal/planner"
	"github.com/tae2089/thread-keep/internal/remote"
	contextstore "github.com/tae2089/thread-keep/internal/store"
	"github.com/zeebo/blake3"
)

const (
	processWebhookJobKind = "process_webhook"
	previewJobKind        = "preview_plan"
	finalJobKind          = "final_plan"
	checkJobKind          = "publish_check"
	jobLease              = 4 * time.Minute
)

type CoordinatorRepository struct {
	RemoteKey           string
	ContextRepositoryID string
	TargetRef           string
	ForgeRepository     string
	InstallationID      int64
	AutomaticLanding    bool
	MaxAttempts         int
}

type CoordinatorConfig struct {
	Refs           *GormRefStore
	Objects        Storage
	Forge          forge.Forge
	CheckPublisher forge.CheckPublisher
	Runner         planner.SourceRunner
	ClaimedRunner  planner.ClaimedSourceRunner
	Repositories   []CoordinatorRepository
}

type Coordinator struct {
	refs           *GormRefStore
	objects        Storage
	forge          forge.Forge
	checkPublisher forge.CheckPublisher
	runner         planner.SourceRunner
	claimedRunner  planner.ClaimedSourceRunner
	refService     *LandingRefService
	ingress        *WebhookIngress
	repositories   map[string]CoordinatorRepository
	byForge        map[string]CoordinatorRepository
	now            func() time.Time
}

type WebhookIntakeResult struct {
	Accepted   bool `json:"accepted"`
	Duplicate  bool `json:"duplicate"`
	Ignored    bool `json:"ignored"`
	Generation int  `json:"generation,omitempty"`
}

type previewJobPayload struct {
	RepositoryKey string           `json:"repository_key"`
	Generation    int              `json:"generation"`
	Change        domain.ChangeKey `json:"change"`
	HeadSHA       string           `json:"head_sha"`
}

type checkJobPayload struct {
	LogicalKey string           `json:"logical_key,omitempty"`
	Version    int              `json:"version,omitempty"`
	Input      forge.CheckInput `json:"input,omitempty"`
}

type finalJobPayload struct {
	RepositoryKey string           `json:"repository_key"`
	LandingID     string           `json:"landing_id"`
	Change        domain.ChangeKey `json:"change"`
}

func NewCoordinator(config CoordinatorConfig) (*Coordinator, error) {
	if config.Refs == nil || config.Objects == nil || config.Forge == nil || len(config.Repositories) == 0 {
		return nil, domain.NewError(domain.CodeValidation, errors.New("coordinator dependencies are incomplete"))
	}
	coordinator := &Coordinator{refs: config.Refs, objects: config.Objects, forge: config.Forge, runner: config.Runner, claimedRunner: config.ClaimedRunner, refService: NewLandingRefService(config.Refs), repositories: make(map[string]CoordinatorRepository, len(config.Repositories)), byForge: make(map[string]CoordinatorRepository, len(config.Repositories)), now: time.Now}
	if config.CheckPublisher == nil {
		publisher, ok := config.Forge.(forge.CheckPublisher)
		if !ok {
			return nil, domain.NewError(domain.CodeValidation, errors.New("coordinator check publisher is not configured"))
		}
		config.CheckPublisher = publisher
	}
	coordinator.checkPublisher = config.CheckPublisher
	for _, repository := range config.Repositories {
		if repository.RemoteKey == "" || repository.ContextRepositoryID == "" || repository.TargetRef == "" || repository.ForgeRepository == "" || repository.InstallationID < 1 || strings.Contains(repository.RemoteKey, "/") {
			return nil, domain.NewError(domain.CodeValidation, errors.New("coordinator repository binding is invalid"))
		}
		if _, exists := coordinator.repositories[repository.RemoteKey]; exists {
			return nil, domain.NewError(domain.CodeValidation, errors.New("coordinator repository key is duplicated"))
		}
		if _, exists := coordinator.byForge[repository.ForgeRepository]; exists {
			return nil, domain.NewError(domain.CodeValidation, errors.New("coordinator forge repository is duplicated"))
		}
		if repository.MaxAttempts == 0 {
			repository.MaxAttempts = 3
		}
		if repository.MaxAttempts < 1 || repository.MaxAttempts > 10 {
			return nil, domain.NewError(domain.CodeValidation, errors.New("coordinator landing max attempts must be between 1 and 10"))
		}
		coordinator.repositories[repository.RemoteKey] = repository
		coordinator.byForge[repository.ForgeRepository] = repository
	}
	ingress, err := NewWebhookIngress(WebhookIngressConfig{Refs: config.Refs, Verifier: config.Forge, Repositories: config.Repositories, Now: coordinator.now})
	if err != nil {
		return nil, err
	}
	coordinator.ingress = ingress
	return coordinator, nil
}

func (c *Coordinator) IntakeWebhook(ctx context.Context, headers http.Header, body []byte) (WebhookIntakeResult, error) {
	return c.ingress.Intake(ctx, headers, body)
}

func (c *Coordinator) CandidateMetadata(ctx context.Context, repositoryKey string, number int) (remote.CandidatePublicationMetadata, error) {
	repository, err := c.repository(repositoryKey)
	if err != nil {
		return remote.CandidatePublicationMetadata{}, err
	}
	changeKey := domain.ChangeKey{Provider: "github", Repository: repository.ForgeRepository, Number: number}
	change, err := c.forge.GetChange(ctx, changeKey)
	if err != nil {
		return remote.CandidatePublicationMetadata{}, err
	}
	if change.State != forge.ChangeOpen {
		return remote.CandidatePublicationMetadata{}, domain.NewError(domain.CodeValidation, errors.New("candidate publication requires an open change"))
	}
	ref, err := c.objects.ReadRef(ctx, repository.ContextRepositoryID, repository.TargetRef)
	if err != nil {
		return remote.CandidatePublicationMetadata{}, err
	}
	if ref.CommitID == "" {
		return remote.CandidatePublicationMetadata{}, domain.NewError(domain.CodeEntityNotFound, errors.New("canonical context ref is not initialized"))
	}
	return remote.CandidatePublicationMetadata{Change: change.Key, BaseSourceSHA: change.BaseSHA, HeadSourceSHA: change.HeadSHA, BaseContextCommitID: ref.CommitID}, nil
}

func (c *Coordinator) PublishCandidate(ctx context.Context, repositoryKey string, request remote.CandidatePublicationRequest) (remote.CandidatePublicationResult, error) {
	repository, err := c.repository(repositoryKey)
	if err != nil {
		return remote.CandidatePublicationResult{}, err
	}
	delta, err := domain.NormalizeCandidateContextDelta(request.Delta)
	if err != nil {
		return remote.CandidatePublicationResult{}, err
	}
	digest, err := domain.CandidateContextDigest(delta)
	if err != nil || digest != request.Digest || delta.Change.Repository != repository.ForgeRepository {
		return remote.CandidatePublicationResult{}, domain.NewError(domain.CodeValidation, errors.New("candidate publication digest or repository is mismatched"))
	}
	change, err := c.forge.GetChange(ctx, delta.Change)
	if err != nil {
		return remote.CandidatePublicationResult{}, err
	}
	if change.State != forge.ChangeOpen || delta.BaseSourceSHA != change.BaseSHA || delta.HeadSourceSHA != change.HeadSHA {
		return remote.CandidatePublicationResult{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("candidate publication does not match the authoritative change generation"))
	}
	ref, err := c.objects.ReadRef(ctx, repository.ContextRepositoryID, repository.TargetRef)
	if err != nil {
		return remote.CandidatePublicationResult{}, err
	}
	if ref.CommitID == "" || delta.BaseContextCommitID != ref.CommitID || ref.SourceSHA != change.BaseSHA {
		return remote.CandidatePublicationResult{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("candidate base context is not the current target context"))
	}
	now := c.now().UTC()
	generation, _, err := c.proposedGeneration(ctx, repositoryKey, change, digest, now)
	if err != nil {
		return remote.CandidatePublicationResult{}, err
	}
	previewJob, err := newPreviewJob(generation, now, 3)
	if err != nil {
		return remote.CandidatePublicationResult{}, err
	}
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change.Key, change.HeadSHA), Change: change.Key, HeadSHA: change.HeadSHA, State: forge.CheckPlanning, Summary: "Context planning is queued for the published candidate.", UpdatedAt: now}
	result, err := c.refs.ScheduleCandidate(ctx, CandidateSchedule{Artifact: StoredCandidateArtifact{Digest: digest, Change: delta.Change, Delta: delta, CreatedAt: now}, Generation: generation, PreviewJob: previewJob, DesiredCheck: desired})
	if err != nil {
		return remote.CandidatePublicationResult{}, err
	}
	return remote.CandidatePublicationResult{Digest: digest, Published: result.ArtifactCreated}, nil
}

func (c *Coordinator) RunOne(ctx context.Context, workerID string, now time.Time) (bool, error) {
	if _, err := c.refs.RepairDesiredChecks(ctx, 100); err != nil {
		return false, err
	}
	job, found, err := c.refs.ClaimJob(ctx, workerID, now, jobLease)
	if err != nil || !found {
		return found, err
	}
	return c.runClaimedJob(ctx, job)
}

func (c *Coordinator) RunOneKinds(ctx context.Context, workerID string, kinds []string, jobTimeout, leaseDuration time.Duration) (bool, error) {
	if jobTimeout <= 0 || leaseDuration <= jobTimeout {
		return false, domain.NewError(domain.CodeValidation, errors.New("coordinator runner timeout ordering is invalid"))
	}
	if containsJobKind(kinds, checkJobKind) {
		if _, err := c.refs.RepairDesiredChecks(ctx, 100); err != nil {
			return false, err
		}
	}
	job, found, err := c.refs.ClaimJobKinds(ctx, JobClaimOptions{WorkerID: workerID, Kinds: kinds, LeaseDuration: leaseDuration})
	if err != nil || !found {
		return found, err
	}
	return c.runClaimedJob(ctx, job)
}

func (c *Coordinator) Reconcile(ctx context.Context) error {
	reconciler, ok := c.claimedRunner.(interface{ Reconcile(context.Context) error })
	if !ok {
		return nil
	}
	return reconciler.Reconcile(ctx)
}

func (c *Coordinator) runClaimedJob(ctx context.Context, job CoordinatorJob) (bool, error) {
	if job.Kind == processWebhookJobKind {
		if err := c.runWebhook(ctx, job); err != nil {
			if abandoned, abandonErr := c.abandonCancelledClaim(ctx, job, err); abandoned {
				return true, abandonErr
			}
			return true, c.handleJobFailure(job, err)
		}
		return true, nil
	}
	if job.Kind == checkJobKind {
		if err := c.runCheck(ctx, job); err != nil {
			if abandoned, abandonErr := c.abandonCancelledClaim(ctx, job, err); abandoned {
				return true, abandonErr
			}
			return true, c.handleJobFailure(job, err)
		}
		return true, nil
	}
	if job.Kind == finalJobKind {
		if err := c.runFinal(ctx, job); err != nil {
			if abandoned, abandonErr := c.abandonCancelledClaim(ctx, job, err); abandoned {
				return true, abandonErr
			}
			return c.handleFinalFailure(job, err)
		}
		return true, nil
	}
	if job.Kind != previewJobKind {
		_ = c.refs.FailClaim(ctx, job.Claim())
		return true, domain.NewError(domain.CodeValidation, errors.New("coordinator job kind is unsupported"))
	}
	if err := c.runPreview(ctx, job); err != nil {
		if abandoned, abandonErr := c.abandonCancelledClaim(ctx, job, err); abandoned {
			return true, abandonErr
		}
		return true, c.handleJobFailure(job, err)
	}
	return true, nil
}

func (c *Coordinator) abandonCancelledClaim(ctx context.Context, job CoordinatorJob, executeErr error) (bool, error) {
	if !errors.Is(ctx.Err(), context.Canceled) || (!errors.Is(executeErr, context.Canceled) && domain.CodeOf(executeErr) != domain.CodeBusy) {
		return false, nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return true, c.refs.AbandonClaim(cleanupCtx, job.Claim(), c.now().UTC())
}

func (c *Coordinator) handleJobFailure(job CoordinatorJob, executeErr error) error {
	if domain.CodeOf(executeErr) == domain.CodeIncompatiblePayload {
		if err := c.refs.RejectClaim(context.Background(), job.Claim(), domain.CodeIncompatiblePayload); err != nil {
			return err
		}
		return nil
	}
	_ = c.refs.FailClaim(context.Background(), job.Claim())
	return executeErr
}

func (c *Coordinator) runWebhook(ctx context.Context, job CoordinatorJob) error {
	var payload processWebhookJobPayload
	if err := UnmarshalDurablePayload(job.Payload, processWebhookJobKind, &payload); err != nil {
		return err
	}
	eventPayload, err := c.refs.WebhookEvent(ctx, payload.Provider, payload.DeliveryID)
	if err != nil {
		return err
	}
	event, err := decodeWebhookEvent(eventPayload)
	if err != nil {
		return err
	}
	if event.Provider != payload.Provider || event.DeliveryID != payload.DeliveryID {
		return domain.NewError(domain.CodeValidation, errors.New("webhook event does not match its process job"))
	}
	repository, found := c.byForge[event.Change.Repository]
	if !found || event.InstallationID != repository.InstallationID || event.BaseRef != strings.TrimPrefix(repository.TargetRef, "refs/contexts/") {
		return domain.NewError(domain.CodeValidation, errors.New("webhook event does not match the coordinator binding"))
	}
	change, err := c.forge.GetChange(ctx, event.Change)
	if err != nil {
		return err
	}
	if change.Key != event.Change || change.BaseRef != strings.TrimPrefix(repository.TargetRef, "refs/contexts/") {
		return domain.NewError(domain.CodeValidation, errors.New("authoritative change does not match the coordinator binding"))
	}
	if change.State == forge.ChangeMerged {
		if !repository.AutomaticLanding {
			return c.completeJob(ctx, job, []byte(`{"outcome":"ignored"}`))
		}
		candidateDigest := ""
		if current, currentErr := c.refs.Generation(ctx, repository.RemoteKey, change.Key); currentErr == nil && current.BaseSourceSHA == change.BaseSHA && current.HeadSourceSHA == change.HeadSHA {
			candidateDigest = current.CandidateDigest
		}
		now := c.now().UTC()
		generation, _, err := c.proposedGeneration(ctx, repository.RemoteKey, change, candidateDigest, now)
		if err != nil {
			return err
		}
		fingerprint := domain.PlanFingerprint{RepositoryID: repository.ContextRepositoryID, TargetRef: repository.TargetRef, Change: change.Key, HeadSourceSHA: change.MergeSHA, CandidateDigest: generation.CandidateDigest}
		intent := domain.LandingIntent{ID: domain.LandingIntentID(fingerprint), RepositoryID: repository.ContextRepositoryID, TargetRef: repository.TargetRef, Change: change.Key, CandidateDigest: generation.CandidateDigest, SourceMergeSHA: change.MergeSHA, PreviewPlanID: generation.CurrentPlanID, State: domain.LandingPending}
		finalJob, err := newFinalJob(repository.RemoteKey, intent, repository.MaxAttempts, now)
		if err != nil {
			return err
		}
		desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change.Key, change.HeadSHA), Change: change.Key, HeadSHA: change.HeadSHA, State: forge.CheckPlanning, Summary: "Source merge is complete; context landing is queued.", UpdatedAt: now}
		if _, err := c.refs.ScheduleLanding(ctx, LandingSchedule{Generation: generation, Intent: intent, FinalJob: finalJob, DesiredCheck: desired}); err != nil {
			return err
		}
		return c.completeJob(ctx, job, []byte(`{"outcome":"scheduled_final"}`))
	}
	if change.State != forge.ChangeOpen {
		return c.completeJob(ctx, job, []byte(`{"outcome":"ignored"}`))
	}
	candidateDigest := ""
	if current, currentErr := c.refs.Generation(ctx, repository.RemoteKey, change.Key); currentErr == nil && current.BaseSourceSHA == change.BaseSHA && current.HeadSourceSHA == change.HeadSHA {
		candidateDigest = current.CandidateDigest
	}
	now := c.now().UTC()
	generation, _, err := c.proposedGeneration(ctx, repository.RemoteKey, change, candidateDigest, now)
	if err != nil {
		return err
	}
	previewJob, err := newPreviewJob(generation, now, 3)
	if err != nil {
		return err
	}
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change.Key, change.HeadSHA), Change: change.Key, HeadSHA: change.HeadSHA, State: forge.CheckPlanning, Summary: "Context planning is queued.", UpdatedAt: now}
	if _, err := c.refs.SchedulePreview(ctx, PreviewSchedule{Generation: generation, PreviewJob: previewJob, DesiredCheck: desired}); err != nil {
		return err
	}
	return c.completeJob(ctx, job, []byte(`{"outcome":"scheduled_preview"}`))
}

func (c *Coordinator) PlanForChange(ctx context.Context, repositoryKey string, change domain.ChangeKey) (domain.ContextPlan, error) {
	generation, err := c.refs.Generation(ctx, repositoryKey, change)
	if err != nil {
		return domain.ContextPlan{}, err
	}
	if generation.CurrentPlanID == "" {
		return domain.ContextPlan{}, domain.NewError(domain.CodeEntityNotFound, errors.New("current context plan is not available"))
	}
	return c.refs.Plan(ctx, repositoryKey, generation.CurrentPlanID)
}

func (c *Coordinator) Plan(ctx context.Context, repositoryKey, planID string) (domain.ContextPlan, error) {
	if _, err := c.repository(repositoryKey); err != nil {
		return domain.ContextPlan{}, err
	}
	return c.refs.Plan(ctx, repositoryKey, planID)
}

func (c *Coordinator) Landings(ctx context.Context, repositoryKey string) ([]domain.LandingIntent, error) {
	repository, err := c.repository(repositoryKey)
	if err != nil {
		return nil, err
	}
	return c.refs.Landings(ctx, repository.ContextRepositoryID, repository.TargetRef)
}

func (c *Coordinator) Landing(ctx context.Context, repositoryKey, landingID string) (domain.LandingIntent, error) {
	repository, err := c.repository(repositoryKey)
	if err != nil {
		return domain.LandingIntent{}, err
	}
	intent, err := c.refs.Landing(ctx, landingID)
	if err != nil {
		return domain.LandingIntent{}, err
	}
	if intent.RepositoryID != repository.ContextRepositoryID || intent.TargetRef != repository.TargetRef {
		return domain.LandingIntent{}, domain.NewError(domain.CodeEntityNotFound, errors.New("landing intent does not belong to this repository"))
	}
	return intent, nil
}

func (c *Coordinator) LandingRecovery(ctx context.Context, repositoryKey, landingID string) (remote.LandingRecoveryBundle, error) {
	intent, err := c.Landing(ctx, repositoryKey, landingID)
	if err != nil {
		return remote.LandingRecoveryBundle{}, err
	}
	if intent.State != domain.LandingBlocked && intent.State != domain.LandingRecovering {
		return remote.LandingRecoveryBundle{}, domain.NewError(domain.CodeValidation, errors.New("landing intent is not available for recovery"))
	}
	repository, _ := c.repository(repositoryKey)
	ref, err := c.objects.ReadRef(ctx, repository.ContextRepositoryID, repository.TargetRef)
	if err != nil {
		return remote.LandingRecoveryBundle{}, err
	}
	candidate := domain.CandidateContextDelta{}
	if intent.CandidateDigest != "" {
		artifact, err := c.refs.Candidate(ctx, intent.CandidateDigest)
		if err != nil {
			return remote.LandingRecoveryBundle{}, err
		}
		candidate = artifact.Delta
	}
	if intent.State == domain.LandingBlocked {
		if _, err := c.refs.TransitionLanding(ctx, intent.ID, domain.LandingBlocked, domain.LandingRecovering); err != nil {
			return remote.LandingRecoveryBundle{}, err
		}
		intent.State = domain.LandingRecovering
	}
	return remote.LandingRecoveryBundle{Intent: intent, Candidate: candidate, ExpectedRef: ref}, nil
}

func (c *Coordinator) runPreview(ctx context.Context, job CoordinatorJob) error {
	if c.runner == nil && c.claimedRunner == nil {
		return domain.NewError(domain.CodeValidation, errors.New("planner execution is not configured in this process"))
	}
	var payload previewJobPayload
	if err := UnmarshalDurablePayload(job.Payload, previewJobKind, &payload); err != nil {
		return err
	}
	repository, err := c.repository(payload.RepositoryKey)
	if err != nil {
		return err
	}
	if payload.Change.Provider != "github" || payload.Change.Repository != repository.ForgeRepository || payload.Change.Number < 1 {
		return domain.NewError(domain.CodeValidation, errors.New("preview job change key is invalid"))
	}
	generation, err := c.refs.Generation(ctx, payload.RepositoryKey, payload.Change)
	if err != nil {
		return err
	}
	if generation.Version != payload.Generation {
		if err := c.publishCheckDurably(ctx, payload.Change, payload.HeadSHA, forge.CheckSuperseded, "A newer pull request generation superseded this context plan.", ""); err != nil {
			return err
		}
		return c.completeJob(ctx, job, []byte(`{"outcome":"superseded"}`))
	}
	grant, err := c.forge.CheckoutGrant(ctx, forge.CheckoutGrantInput{Change: generation.Change})
	if err != nil {
		return err
	}
	evidence, err := c.indexClaimedSource(ctx, job, planner.SourceRequest{Mode: planner.SourcePreview, RepositoryID: repository.ContextRepositoryID, TargetRef: repository.TargetRef, RepositoryURL: grant.CloneURL, Credential: grant.Token, BaseSHA: generation.BaseSourceSHA, HeadSHA: generation.HeadSourceSHA})
	grant.Token = ""
	if err != nil {
		return err
	}
	ref, canonical, err := c.canonicalContext(ctx, repository)
	if err != nil {
		return err
	}
	candidate := domain.CandidateContextDelta{}
	if generation.CandidateDigest != "" {
		artifact, err := c.refs.Candidate(ctx, generation.CandidateDigest)
		if err != nil {
			return err
		}
		candidate = artifact.Delta
	}
	fingerprint := domain.PlanFingerprint{RepositoryID: repository.ContextRepositoryID, TargetRef: repository.TargetRef, Change: generation.Change, BaseSourceSHA: generation.BaseSourceSHA, HeadSourceSHA: generation.HeadSourceSHA, SourceEvidenceDigest: evidence.EntityShapeDigest, BaseContextCommitID: ref.CommitID, BaseContextVersion: ref.Version, CandidateDigest: generation.CandidateDigest, ProvenanceDigest: domain.DigestProvenance(evidence.Provenance)}
	plan, err := domain.PlanLanding(domain.LandingPlanInput{Kind: domain.ContextPlanPreview, Fingerprint: fingerprint, Canonical: canonical, TargetEntities: evidence.Entities, TargetProvenance: evidence.Provenance, CoverageComplete: evidence.CoverageComplete, Candidate: candidate, CreatedAt: c.now().UTC()})
	if err != nil {
		return err
	}
	result, _ := json.Marshal(map[string]string{"plan_id": plan.ID, "outcome": string(plan.Outcome)})
	state := checkStateForPlan(plan.Outcome)
	summary := fmt.Sprintf("Plan %s: %d active, %d review, %d conflicts.", plan.Outcome, plan.Summary.ActiveNotes, plan.Summary.NeedsReviewNotes, plan.Summary.Conflicts)
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(generation.Change, generation.HeadSourceSHA), Change: generation.Change, HeadSHA: generation.HeadSourceSHA, State: state, Summary: summary, PlanURL: "/plans/" + plan.ID, UpdatedAt: c.now().UTC()}
	return c.refs.PublishPreviewResult(ctx, generation, plan, desired, job, result)
}

func (c *Coordinator) indexClaimedSource(ctx context.Context, job CoordinatorJob, request planner.SourceRequest) (planner.SourceEvidence, error) {
	if c.claimedRunner != nil {
		return c.claimedRunner.IndexClaimedSource(ctx, planner.RunnerClaim{JobID: job.ID, LeaseOwner: job.LeaseOwner, FencingToken: job.FencingToken}, request)
	}
	return c.runner.IndexSource(ctx, request)
}

func (c *Coordinator) runFinal(ctx context.Context, job CoordinatorJob) error {
	if c.runner == nil && c.claimedRunner == nil {
		return domain.NewError(domain.CodeValidation, errors.New("planner execution is not configured in this process"))
	}
	var payload finalJobPayload
	if err := UnmarshalDurablePayload(job.Payload, finalJobKind, &payload); err != nil {
		return err
	}
	repository, err := c.repository(payload.RepositoryKey)
	if err != nil {
		return err
	}
	intent, err := c.refs.Landing(ctx, payload.LandingID)
	if err != nil {
		return err
	}
	if intent.State == domain.LandingLanded {
		return c.completeJob(ctx, job, []byte(`{"outcome":"already_landed"}`))
	}
	switch intent.State {
	case domain.LandingPending:
		intent, err = c.refs.TransitionLanding(ctx, intent.ID, domain.LandingPending, domain.LandingRunning)
	case domain.LandingRetryable:
		intent, err = c.refs.TransitionLanding(ctx, intent.ID, domain.LandingRetryable, domain.LandingRunning)
	case domain.LandingRunning:
	default:
		return domain.NewError(domain.CodeValidation, errors.New("landing intent is not runnable"))
	}
	if err != nil {
		return err
	}
	change, err := c.forge.GetChange(ctx, payload.Change)
	if err != nil {
		return err
	}
	if change.State != forge.ChangeMerged || !change.Merged || change.MergeSHA != intent.SourceMergeSHA {
		return domain.NewError(domain.CodeStaleWorkingSet, errors.New("authoritative merged change does not match the landing intent"))
	}
	grant, err := c.forge.CheckoutGrant(ctx, forge.CheckoutGrantInput{Change: change.Key})
	if err != nil {
		return err
	}
	evidence, err := c.indexClaimedSource(ctx, job, planner.SourceRequest{Mode: planner.SourceFinal, RepositoryID: repository.ContextRepositoryID, TargetRef: repository.TargetRef, RepositoryURL: grant.CloneURL, Credential: grant.Token, FinalSHA: intent.SourceMergeSHA})
	grant.Token = ""
	if err != nil {
		return err
	}
	ref, canonical, err := c.canonicalContext(ctx, repository)
	if err != nil {
		return err
	}
	generation, err := c.refs.Generation(ctx, payload.RepositoryKey, change.Key)
	if err != nil {
		return err
	}
	candidate := domain.CandidateContextDelta{}
	if intent.CandidateDigest != "" {
		artifact, err := c.refs.Candidate(ctx, intent.CandidateDigest)
		if err != nil {
			return err
		}
		candidate = artifact.Delta
	}
	fingerprint := domain.PlanFingerprint{RepositoryID: repository.ContextRepositoryID, TargetRef: repository.TargetRef, Change: change.Key, BaseSourceSHA: canonical.SourceSHA, HeadSourceSHA: intent.SourceMergeSHA, SourceEvidenceDigest: evidence.EntityShapeDigest, BaseContextCommitID: ref.CommitID, BaseContextVersion: ref.Version, CandidateDigest: intent.CandidateDigest, ProvenanceDigest: domain.DigestProvenance(evidence.Provenance)}
	plan, err := domain.PlanLanding(domain.LandingPlanInput{Kind: domain.ContextPlanFinal, Fingerprint: fingerprint, Canonical: canonical, TargetEntities: evidence.Entities, TargetProvenance: evidence.Provenance, CoverageComplete: evidence.CoverageComplete, Candidate: candidate, CreatedAt: c.now().UTC()})
	if err != nil {
		return err
	}
	if err := c.refs.SaveCurrentPlan(ctx, generation, plan); err != nil {
		return err
	}
	if err := c.refs.SetLandingPlan(ctx, intent.ID, plan.ID); err != nil {
		return err
	}
	if plan.Outcome == domain.ContextPlanBlocked {
		result, _ := json.Marshal(map[string]string{"plan_id": plan.ID, "outcome": "blocked"})
		desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change.Key, change.HeadSHA), Change: change.Key, HeadSHA: change.HeadSHA, State: forge.CheckBlocked, Summary: "Context landing is blocked; local recovery is available.", PlanURL: "/plans/" + plan.ID, UpdatedAt: c.now().UTC()}
		return c.refs.CommitBlockedLanding(ctx, BlockedLandingCommit{Claim: job.Claim(), LandingID: intent.ID, ExpectedState: domain.LandingRunning, ErrorCode: domain.CodeValidation, DesiredCheck: desired, ResultPayload: result})
	}
	snapshot, err := domain.BuildLandingSnapshot(domain.LandingBuildInput{Plan: plan, ParentID: ref.CommitID, Entities: evidence.Entities, Provenance: evidence.Provenance, Message: "Land context for " + change.Key.Repository + "#" + strconv.Itoa(change.Key.Number), Author: "thread-keep", CreatedAt: c.now().UTC(), Resolver: "automatic"})
	if err != nil {
		return err
	}
	contents, err := json.Marshal(snapshot)
	if err != nil {
		return domain.NewError(domain.CodeValidation, errors.New("serialize landing snapshot"))
	}
	digest := blake3.Sum256(contents)
	objectID := fmt.Sprintf("%x", digest[:])
	if _, err := c.objects.PublishObject(ctx, repository.ContextRepositoryID, objectID, contents); err != nil {
		return err
	}
	next := remote.Ref{RefName: repository.TargetRef, CommitID: objectID, SourceSHA: intent.SourceMergeSHA, Version: ref.Version + 1}
	state := checkStateForPlan(plan.Outcome)
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change.Key, change.HeadSHA), Change: change.Key, HeadSHA: change.HeadSHA, State: state, Summary: "Context landing completed at " + objectID[:12] + ".", PlanURL: "/plans/" + plan.ID, UpdatedAt: c.now().UTC()}
	if _, err := c.refService.AdvanceWithCheck(ctx, RefAdvanceInput{Expected: ref, Next: next, Object: snapshot}, desired); err != nil {
		return err
	}
	result, _ := json.Marshal(map[string]string{"plan_id": plan.ID, "context_commit_id": objectID, "outcome": "landed"})
	return c.completeJob(ctx, job, result)
}

func (c *Coordinator) handleFinalFailure(job CoordinatorJob, executeErr error) (bool, error) {
	if domain.CodeOf(executeErr) == domain.CodeIncompatiblePayload {
		if err := c.refs.RejectClaim(context.Background(), job.Claim(), domain.CodeIncompatiblePayload); err != nil {
			return true, err
		}
		return true, nil
	}
	var payload finalJobPayload
	if UnmarshalDurablePayload(job.Payload, finalJobKind, &payload) != nil || payload.LandingID == "" {
		_ = c.refs.FailClaim(context.Background(), job.Claim())
		return true, executeErr
	}
	code := domain.CodeOf(executeErr)
	if code == "" {
		code = domain.CodeLocalStorage
	}
	if code == domain.CodeValidation || code == domain.CodeCoverageIncomplete || code == domain.CodeAuth || code == domain.CodeStaleWorkingSet || code == domain.CodeEntityNotFound {
		intent, intentErr := c.refs.Landing(context.Background(), payload.LandingID)
		generation, generationErr := c.refs.Generation(context.Background(), payload.RepositoryKey, payload.Change)
		if intentErr != nil || generationErr != nil || intent.State != domain.LandingRunning {
			_ = c.refs.FailClaim(context.Background(), job.Claim())
			return true, executeErr
		}
		planURL := ""
		if intent.FinalPlanID != "" {
			planURL = "/plans/" + intent.FinalPlanID
		}
		desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(payload.Change, generation.HeadSourceSHA), Change: payload.Change, HeadSHA: generation.HeadSourceSHA, State: forge.CheckBlocked, Summary: "Context landing stopped permanently: " + string(code) + ".", PlanURL: planURL, UpdatedAt: c.now().UTC()}
		if err := c.refs.CommitBlockedLanding(context.Background(), BlockedLandingCommit{Claim: job.Claim(), LandingID: payload.LandingID, ExpectedState: domain.LandingRunning, ErrorCode: code, DesiredCheck: desired, ResultPayload: []byte(`{"outcome":"blocked"}`)}); err != nil {
			return true, err
		}
		return true, nil
	}
	repository, repositoryErr := c.repository(payload.RepositoryKey)
	if repositoryErr != nil {
		_ = c.refs.FailClaim(context.Background(), job.Claim())
		return true, executeErr
	}
	generation, generationErr := c.refs.Generation(context.Background(), payload.RepositoryKey, payload.Change)
	if generationErr != nil {
		_ = c.refs.FailClaim(context.Background(), job.Claim())
		return true, executeErr
	}
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(payload.Change, generation.HeadSourceSHA), Change: payload.Change, HeadSHA: generation.HeadSourceSHA, State: forge.CheckBlocked, Summary: "Context landing retries were exhausted: " + string(code) + ".", UpdatedAt: c.now().UTC()}
	intent, recordErr := c.refs.CommitLandingFailure(context.Background(), LandingFailureCommit{Claim: job.Claim(), LandingID: payload.LandingID, ErrorCode: code, Now: c.now().UTC(), MaxAttempts: repository.MaxAttempts, BlockedCheck: desired, ResultPayload: []byte(`{"outcome":"blocked"}`)})
	if recordErr != nil {
		return true, recordErr
	}
	if intent.State == domain.LandingBlocked {
		return true, nil
	}
	return true, executeErr
}

func (c *Coordinator) canonicalContext(ctx context.Context, repository CoordinatorRepository) (remote.Ref, domain.ContextObject, error) {
	ref, err := c.objects.ReadRef(ctx, repository.ContextRepositoryID, repository.TargetRef)
	if err != nil {
		return remote.Ref{}, domain.ContextObject{}, err
	}
	if ref.CommitID == "" {
		return remote.Ref{}, domain.ContextObject{}, domain.NewError(domain.CodeEntityNotFound, errors.New("canonical context ref is not initialized"))
	}
	contents, err := c.objects.ReadObject(ctx, repository.ContextRepositoryID, ref.CommitID)
	if err != nil {
		return remote.Ref{}, domain.ContextObject{}, err
	}
	var object domain.ContextObject
	if err := json.Unmarshal(contents, &object); err != nil || object.SourceSHA != ref.SourceSHA {
		return remote.Ref{}, domain.ContextObject{}, domain.NewError(domain.CodeValidation, errors.New("canonical context object is invalid or mismatched"))
	}
	if err := contextstore.ValidateContextObject(object, repository.ContextRepositoryID, repository.TargetRef); err != nil {
		return remote.Ref{}, domain.ContextObject{}, err
	}
	return ref, object, nil
}

func (c *Coordinator) proposedGeneration(ctx context.Context, repositoryKey string, change forge.Change, candidateDigest string, now time.Time) (PRGeneration, bool, error) {
	current, err := c.refs.Generation(ctx, repositoryKey, change.Key)
	if err != nil && domain.CodeOf(err) != domain.CodeEntityNotFound {
		return PRGeneration{}, false, err
	}
	if err == nil && current.BaseSourceSHA == change.BaseSHA && current.HeadSourceSHA == change.HeadSHA && current.CandidateDigest == candidateDigest {
		return current, false, nil
	}
	version := 1
	if err == nil {
		version = current.Version + 1
	}
	generation := PRGeneration{RepositoryKey: repositoryKey, Change: change.Key, BaseSourceSHA: change.BaseSHA, HeadSourceSHA: change.HeadSHA, CandidateDigest: candidateDigest, Version: version, UpdatedAt: now.UTC()}
	return generation, true, nil
}

func newPreviewJob(generation PRGeneration, now time.Time, maxAttempts int) (CoordinatorJob, error) {
	payload, err := MarshalDurablePayload(previewJobKind, previewJobPayload{RepositoryKey: generation.RepositoryKey, Generation: generation.Version, Change: generation.Change, HeadSHA: generation.HeadSourceSHA})
	if err != nil {
		return CoordinatorJob{}, err
	}
	dedupe := "preview:" + generation.RepositoryKey + ":" + strconv.Itoa(generation.Change.Number) + ":" + strconv.Itoa(generation.Version)
	digest := blake3.Sum256([]byte(dedupe))
	return CoordinatorJob{ID: fmt.Sprintf("%x", digest[:]), DedupeKey: dedupe, Kind: previewJobKind, Priority: 100, Payload: payload, State: CoordinatorJobPending, MaxAttempts: maxAttempts, NextAttemptAt: now.UTC()}, nil
}

func newFinalJob(repositoryKey string, intent domain.LandingIntent, maxAttempts int, now time.Time) (CoordinatorJob, error) {
	payload, err := MarshalDurablePayload(finalJobKind, finalJobPayload{RepositoryKey: repositoryKey, LandingID: intent.ID, Change: intent.Change})
	if err != nil {
		return CoordinatorJob{}, err
	}
	dedupe := "final:" + intent.ID
	digest := blake3.Sum256([]byte(dedupe))
	return CoordinatorJob{ID: fmt.Sprintf("%x", digest[:]), DedupeKey: dedupe, Kind: finalJobKind, Priority: 200, Payload: payload, State: CoordinatorJobPending, MaxAttempts: maxAttempts, NextAttemptAt: now.UTC()}, nil
}

func (c *Coordinator) publishCheckDurably(ctx context.Context, change domain.ChangeKey, headSHA string, state forge.CheckState, summary, planURL string) error {
	desired := DesiredCheck{LogicalKey: DesiredCheckLogicalKey(change, headSHA), Change: change, HeadSHA: headSHA, State: state, Summary: summary, PlanURL: planURL, UpdatedAt: c.now().UTC()}
	_, _, err := c.refs.SetDesiredCheck(ctx, desired)
	return err
}

func (c *Coordinator) runCheck(ctx context.Context, job CoordinatorJob) error {
	var payload checkJobPayload
	if err := UnmarshalDurablePayload(job.Payload, checkJobKind, &payload); err != nil {
		return err
	}
	desired, err := c.refs.DesiredCheck(ctx, payload.LogicalKey)
	if err != nil {
		return err
	}
	if payload.Version < desired.Version {
		return c.refs.SupersedeCheckClaim(ctx, job.Claim(), payload.LogicalKey, payload.Version, []byte(`{"outcome":"superseded"}`))
	}
	if payload.Version != desired.Version {
		return domain.NewError(domain.CodeValidation, errors.New("check job version is invalid"))
	}
	if err := c.refs.ValidateClaim(ctx, job.Claim()); err != nil {
		return err
	}
	publication, err := c.checkPublisher.ReconcileCheck(ctx, forge.CheckInput{Change: desired.Change, HeadSHA: desired.HeadSHA, State: desired.State, Summary: desired.Summary, PlanURL: desired.PlanURL, CheckRunID: desired.ProviderCheckRunID})
	if err != nil {
		return err
	}
	return c.refs.CommitCheckPublication(ctx, CheckPublicationCommit{Claim: job.Claim(), LogicalKey: desired.LogicalKey, DesiredVersion: desired.Version, ProviderCheckRunID: publication.CheckRunID, ResultPayload: []byte(`{"outcome":"published"}`)})
}

func (c *Coordinator) completeJob(ctx context.Context, job CoordinatorJob, result []byte) error {
	return c.refs.CompleteClaim(ctx, job.Claim(), result)
}

func (c *Coordinator) repository(key string) (CoordinatorRepository, error) {
	repository, found := c.repositories[key]
	if !found {
		return CoordinatorRepository{}, domain.NewError(domain.CodeEntityNotFound, errors.New("coordinator repository is not configured"))
	}
	return repository, nil
}

func checkStateForPlan(outcome domain.ContextPlanOutcome) forge.CheckState {
	switch outcome {
	case domain.ContextPlanReady:
		return forge.CheckReady
	case domain.ContextPlanReviewRequired:
		return forge.CheckReviewRequired
	default:
		return forge.CheckBlocked
	}
}

func containsJobKind(kinds []string, wanted string) bool {
	for _, kind := range kinds {
		if kind == wanted {
			return true
		}
	}
	return false
}
