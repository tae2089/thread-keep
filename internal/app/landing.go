package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/remote"
	"github.com/tae2089/thread-keep/internal/store"
	"github.com/zeebo/blake3"
)

type LandingResolveInput struct {
	SessionID  string
	ConflictID string
	Use        string
	Authored   *domain.Note
}

type LandingCommitInput struct {
	SessionID string
	Message   string
	Author    string
}

func (s *Service) Landings(ctx context.Context, remoteName string) ([]domain.LandingIntent, error) {
	transport, err := s.landingTransport(ctx, remoteName)
	if err != nil {
		return nil, err
	}
	return transport.Landings(ctx)
}

func (s *Service) Landing(ctx context.Context, remoteName, landingID string) (domain.LandingIntent, error) {
	transport, err := s.landingTransport(ctx, remoteName)
	if err != nil {
		return domain.LandingIntent{}, err
	}
	return transport.Landing(ctx, strings.TrimSpace(landingID))
}

func (s *Service) LandingSession(ctx context.Context, sessionID string) (domain.LandingSession, error) {
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.LandingSession{}, err
	}
	return contextStore.LandingSession(ctx, sessionID)
}

func (s *Service) RecoverLanding(ctx context.Context, remoteName, landingID string) (domain.LandingSession, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.LandingSession{}, err
	}
	if err := state.RequireCleanWorktree(ctx); err != nil {
		return domain.LandingSession{}, err
	}
	if _, err := s.UpdateWithOptions(ctx, true); err != nil {
		return domain.LandingSession{}, err
	}
	state, key, err = s.mutableKey(ctx)
	if err != nil {
		return domain.LandingSession{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.LandingSession{}, err
	}
	snapshot, err := contextStore.CommitSnapshot(ctx, key)
	if err != nil {
		return domain.LandingSession{}, err
	}
	if len(snapshot.PendingNotes) != 0 {
		return domain.LandingSession{}, domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit or discard pending context changes before landing recovery"))
	}
	transport, err := s.landingTransport(ctx, remoteName)
	if err != nil {
		return domain.LandingSession{}, err
	}
	bundle, err := transport.LandingRecovery(ctx, strings.TrimSpace(landingID))
	if err != nil {
		return domain.LandingSession{}, err
	}
	if err := validateRecoveryBundle(bundle, key, state.HeadSHA); err != nil {
		return domain.LandingSession{}, err
	}
	configured, err := contextStore.Remote(ctx, remoteName)
	if err != nil {
		return domain.LandingSession{}, err
	}
	objectTransport, err := remote.Dial(configured.Path)
	if err != nil {
		return domain.LandingSession{}, err
	}
	actualRemote, err := objectTransport.ReadRef(ctx, key.RefName)
	if err != nil {
		return domain.LandingSession{}, err
	}
	if actualRemote != bundle.ExpectedRef {
		return domain.LandingSession{}, domain.NewError(domain.CodeRemoteConflict, errors.New("canonical context ref changed while starting landing recovery"))
	}
	imported, err := contextStore.ImportObjectChain(ctx, actualRemote.CommitID, key.RepositoryID, key.RefName, objectTransport)
	if err != nil {
		return domain.LandingSession{}, err
	}
	if imported.SourceSHA != actualRemote.SourceSHA {
		return domain.LandingSession{}, domain.NewError(domain.CodeValidation, errors.New("landing recovery ref source does not match its immutable object"))
	}
	if err := contextStore.RecordRemoteRef(ctx, domain.RemoteRef{RemoteName: configured.Name, RefName: actualRemote.RefName, CommitID: actualRemote.CommitID, SourceSHA: actualRemote.SourceSHA, Version: actualRemote.Version}); err != nil {
		return domain.LandingSession{}, err
	}
	local, err := contextStore.ContextRef(ctx, key)
	if err != nil {
		return domain.LandingSession{}, err
	}
	fastForward, err := contextStore.IsAncestor(actualRemote.CommitID, local.CommitID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.LandingSession{}, err
	}
	if !fastForward {
		return domain.LandingSession{}, domain.NewError(domain.CodeRemoteConflict, errors.New("canonical context does not fast-forward the local context ref"))
	}
	if local.CommitID != actualRemote.CommitID || local.SourceSHA != actualRemote.SourceSHA {
		next := domain.ContextRef{RefName: actualRemote.RefName, CommitID: actualRemote.CommitID, SourceSHA: actualRemote.SourceSHA, Version: actualRemote.Version}
		if err := contextStore.PrepareLandingRecovery(ctx, store.PrepareLandingRecoveryInput{Key: key, Expected: local, Next: next}); err != nil {
			return domain.LandingSession{}, err
		}
	}
	canonical, err := contextStore.ReadContextObject(actualRemote.CommitID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.LandingSession{}, err
	}
	status, err := contextStore.Status(ctx, key)
	if err != nil {
		return domain.LandingSession{}, err
	}
	provenance, err := contextSnapshotProvenance(status.Coverage, state.HeadSHA)
	if err != nil {
		return domain.LandingSession{}, err
	}
	candidateDigest, err := recoveryCandidateDigest(bundle.Candidate)
	if err != nil {
		return domain.LandingSession{}, err
	}
	fingerprint := domain.PlanFingerprint{RepositoryID: key.RepositoryID, TargetRef: key.RefName, Change: bundle.Intent.Change, BaseSourceSHA: canonical.SourceSHA, HeadSourceSHA: state.HeadSHA, SourceEvidenceDigest: domain.DigestSourceEvidence(snapshot.Entities), BaseContextCommitID: actualRemote.CommitID, BaseContextVersion: actualRemote.Version, CandidateDigest: candidateDigest, ProvenanceDigest: domain.DigestProvenance(provenance)}
	if bundle.Intent.CandidateDigest != candidateDigest || bundle.Intent.ID != domain.LandingIntentID(fingerprint) {
		return domain.LandingSession{}, domain.NewError(domain.CodeValidation, errors.New("landing recovery intent does not match its immutable evidence"))
	}
	plan, err := domain.PlanLanding(domain.LandingPlanInput{Kind: domain.ContextPlanFinal, Fingerprint: fingerprint, Canonical: canonical, TargetEntities: snapshot.Entities, TargetProvenance: provenance, CoverageComplete: status.CoverageComplete, Candidate: bundle.Candidate, CreatedAt: time.Now().UTC()})
	if err != nil {
		return domain.LandingSession{}, err
	}
	session := domain.LandingSession{LandingID: bundle.Intent.ID, RemoteName: configured.Name, RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: state.HeadSHA, ExpectedRemoteCommitID: actualRemote.CommitID, ExpectedRemoteRefVersion: actualRemote.Version, Plan: plan, Candidate: bundle.Candidate, Entities: append([]domain.Entity(nil), snapshot.Entities...), Provenance: provenance, CreatedAt: time.Now().UTC()}
	return contextStore.CreateLandingSession(ctx, session)
}

func (s *Service) ResolveLanding(ctx context.Context, input LandingResolveInput) (domain.LandingSession, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.LandingSession{}, err
	}
	if err := state.RequireCleanWorktree(ctx); err != nil {
		return domain.LandingSession{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.LandingSession{}, err
	}
	session, err := contextStore.LandingSession(ctx, input.SessionID)
	if err != nil {
		return domain.LandingSession{}, err
	}
	if session.RepositoryID != key.RepositoryID || session.RefName != key.RefName || session.SourceSHA != state.HeadSHA || session.State == domain.LandingSessionCommitted {
		return domain.LandingSession{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("landing session does not match the current Git source"))
	}
	plan, err := domain.ResolveLandingPlan(session.Plan, session.Candidate, session.Entities, strings.TrimSpace(input.ConflictID), domain.LandingResolutionUse(strings.TrimSpace(input.Use)), input.Authored)
	if err != nil {
		return domain.LandingSession{}, err
	}
	session.Plan = plan
	return contextStore.UpdateLandingSession(ctx, session.Version, session)
}

func (s *Service) CommitLanding(ctx context.Context, input LandingCommitInput) (domain.ContextCommit, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if err := state.RequireCleanWorktree(ctx); err != nil {
		return domain.ContextCommit{}, err
	}
	message := strings.TrimSpace(input.Message)
	if message == "" {
		return domain.ContextCommit{}, domain.NewError(domain.CodeValidation, errors.New("landing commit message must not be empty"))
	}
	author := strings.TrimSpace(input.Author)
	if author == "" {
		author = defaultAuthor(ctx, state)
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	session, err := contextStore.LandingSession(ctx, input.SessionID)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if session.State != domain.LandingSessionReady || session.RepositoryID != key.RepositoryID || session.RefName != key.RefName || session.SourceSHA != state.HeadSHA {
		return domain.ContextCommit{}, domain.NewError(domain.CodeValidation, errors.New("landing session is not ready for the current Git source"))
	}
	snapshot, err := contextStore.CommitSnapshot(ctx, key)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if snapshot.ParentID != session.ExpectedRemoteCommitID || snapshot.WorkingSource != state.HeadSHA || len(snapshot.PendingNotes) != 0 {
		return domain.ContextCommit{}, domain.NewError(domain.CodeConcurrentUpdate, errors.New("landing recovery working set changed before commit"))
	}
	configured, err := contextStore.Remote(ctx, session.RemoteName)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	transport, err := remote.Dial(configured.Path)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	remoteRef, err := transport.ReadRef(ctx, key.RefName)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	expectedRemote := remote.Ref{RefName: key.RefName, CommitID: session.ExpectedRemoteCommitID, SourceSHA: session.Plan.Fingerprint.BaseSourceSHA, Version: session.ExpectedRemoteRefVersion}
	if remoteRef != expectedRemote {
		return domain.ContextCommit{}, domain.NewError(domain.CodeRemoteConflict, errors.New("canonical context ref changed since landing recovery started"))
	}
	createdAt := time.Now().UTC()
	object, err := domain.BuildLandingSnapshot(domain.LandingBuildInput{Plan: session.Plan, ParentID: session.ExpectedRemoteCommitID, Entities: session.Entities, Provenance: session.Provenance, Message: message, Author: author, CreatedAt: createdAt, Resolver: "manual"})
	if err != nil {
		return domain.ContextCommit{}, err
	}
	sort.Slice(object.Entities, func(i, j int) bool { return object.Entities[i].Key < object.Entities[j].Key })
	contents, err := json.Marshal(object)
	if err != nil {
		return domain.ContextCommit{}, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("serialize landing context object: %w", err))
	}
	digest := blake3.Sum256(contents)
	identifier := fmt.Sprintf("%x", digest[:])
	if err := s.requireCommitSourceState(ctx, state); err != nil {
		return domain.ContextCommit{}, err
	}
	if err := contextStore.WriteObject(identifier, contents); err != nil {
		return domain.ContextCommit{}, err
	}
	if err := s.requireCommitSourceState(ctx, state); err != nil {
		return domain.ContextCommit{}, err
	}
	commit := domain.ContextCommit{ID: identifier, ParentID: session.ExpectedRemoteCommitID, RefName: key.RefName, SourceSHA: state.HeadSHA, Message: message, Author: author, CreatedAt: createdAt}
	notes := append([]domain.Note(nil), object.Notes...)
	if err := contextStore.FinalizeLanding(ctx, store.FinalizeLandingInput{Key: key, Session: session, ExpectedParent: session.ExpectedRemoteCommitID, Commit: commit, Notes: notes}); err != nil {
		return domain.ContextCommit{}, err
	}
	return commit, nil
}

func (s *Service) landingTransport(ctx context.Context, remoteName string) (remote.LandingTransport, error) {
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return nil, err
	}
	configured, err := contextStore.Remote(ctx, remoteName)
	if err != nil {
		return nil, err
	}
	transport, err := remote.Dial(configured.Path)
	if err != nil {
		return nil, err
	}
	landing, ok := transport.(remote.LandingTransport)
	if !ok {
		return nil, domain.NewError(domain.CodeValidation, errors.New("remote does not support landing coordination"))
	}
	return landing, nil
}

func validateRecoveryBundle(bundle remote.LandingRecoveryBundle, key domain.WorkingSetKey, sourceSHA string) error {
	intent := bundle.Intent
	if intent.ID == "" || (intent.State != domain.LandingBlocked && intent.State != domain.LandingRecovering) || intent.RepositoryID != key.RepositoryID || intent.TargetRef != key.RefName || intent.SourceMergeSHA != sourceSHA || bundle.ExpectedRef.RefName != key.RefName || bundle.ExpectedRef.CommitID == "" || bundle.ExpectedRef.SourceSHA == "" || bundle.ExpectedRef.Version < 1 {
		return domain.NewError(domain.CodeValidation, errors.New("landing recovery bundle does not match the current repository, ref, and merge source"))
	}
	return nil
}

func recoveryCandidateDigest(candidate domain.CandidateContextDelta) (string, error) {
	if candidate.SchemaVersion == 0 && len(candidate.Records) == 0 {
		return "", nil
	}
	return domain.CandidateContextDigest(candidate)
}
