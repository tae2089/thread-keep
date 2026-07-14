package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/tae2089/thread-keep/internal/store"
	"github.com/zeebo/blake3"
)

func (s *Service) StartMerge(ctx context.Context, input MergeStartInput) (domain.MergeSession, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.MergeSession{}, err
	}
	if err := state.RequireCleanWorktree(ctx); err != nil {
		return domain.MergeSession{}, err
	}
	localID, err := domain.NormalizeContextCommitID(input.LocalSnapshotID)
	if err != nil {
		return domain.MergeSession{}, err
	}
	remoteID, err := domain.NormalizeContextCommitID(input.RemoteSnapshotID)
	if err != nil {
		return domain.MergeSession{}, err
	}
	if localID == remoteID {
		return domain.MergeSession{}, domain.NewError(domain.CodeValidation, errors.New("merge snapshots must be distinct"))
	}
	message := strings.TrimSpace(input.Message)
	author := strings.TrimSpace(input.Author)
	if message == "" || author == "" {
		return domain.MergeSession{}, domain.NewError(domain.CodeValidation, errors.New("merge message and author must not be empty"))
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.MergeSession{}, err
	}
	ref, err := contextStore.ContextRef(ctx, key)
	if err != nil {
		return domain.MergeSession{}, err
	}
	if ref.CommitID != localID {
		return domain.MergeSession{}, domain.NewError(domain.CodeConcurrentUpdate, errors.New("merge local snapshot does not match the current context ref"))
	}
	pending, err := contextStore.PendingNotes(ctx, key)
	if err != nil {
		return domain.MergeSession{}, err
	}
	if len(pending) != 0 {
		return domain.MergeSession{}, domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit or discard pending context changes before merging"))
	}
	local, err := contextStore.ReadContextObject(localID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.MergeSession{}, err
	}
	remote, err := contextStore.ReadContextObject(remoteID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.MergeSession{}, err
	}
	if !domain.IsContextSnapshotSchema(local.SchemaVersion) || !domain.IsContextSnapshotSchema(remote.SchemaVersion) || local.SourceSHA != key.SourceSHA || remote.SourceSHA != key.SourceSHA {
		return domain.MergeSession{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("merge snapshots must match the current Git source"))
	}
	baseID, err := contextStore.FindMergeBase(localID, remoteID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.MergeSession{}, err
	}
	base, err := contextStore.ReadContextObject(baseID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.MergeSession{}, err
	}
	plan, err := domain.PlanSnapshotMerge(domain.SnapshotMergeInput{Base: base, Local: local, Remote: remote})
	if err != nil {
		return domain.MergeSession{}, err
	}
	conflicts := make([]domain.MergeSessionConflict, 0, len(plan.Conflicts))
	for _, conflict := range plan.Conflicts {
		conflicts = append(conflicts, domain.MergeSessionConflict{ID: conflict.NoteID, SnapshotMergeConflict: conflict, Resolution: domain.MergeConflictUnresolved})
	}
	return contextStore.CreateMergeSession(ctx, domain.MergeSession{LocalSnapshotID: localID, RemoteSnapshotID: remoteID, BaseSnapshotID: baseID, RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: key.SourceSHA, Provenance: local.Provenance, Message: message, Author: author, PlannedCreatedAt: time.Now().UTC(), AutomaticRecords: plan.Records, Conflicts: conflicts})
}

func (s *Service) MergeSession(ctx context.Context, sessionID string) (domain.MergeSession, error) {
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.MergeSession{}, err
	}
	return contextStore.MergeSession(ctx, sessionID)
}

func (s *Service) ResolveMerge(ctx context.Context, input MergeResolveInput) (domain.MergeSession, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.MergeSession{}, err
	}
	if err := state.RequireCleanWorktree(ctx); err != nil {
		return domain.MergeSession{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.MergeSession{}, err
	}
	session, err := contextStore.MergeSession(ctx, input.SessionID)
	if err != nil {
		return domain.MergeSession{}, err
	}
	if session.RepositoryID != key.RepositoryID || session.RefName != key.RefName || session.SourceSHA != key.SourceSHA {
		return domain.MergeSession{}, domain.NewError(domain.CodeStaleWorkingSet, errors.New("merge session does not match the current Git source"))
	}
	resolution := domain.MergeConflictResolution(strings.TrimSpace(input.Use))
	var authored *domain.SnapshotMergeRecord
	if resolution == domain.MergeConflictAuthored {
		authored, err = authoredMergeRecord(session, input)
		if err != nil {
			return domain.MergeSession{}, err
		}
	}
	return contextStore.ResolveMergeConflict(ctx, input.SessionID, input.ConflictID, resolution, authored)
}

func (s *Service) CommitMerge(ctx context.Context, sessionID string) (domain.ContextCommit, error) {
	state, key, err := s.mutableKey(ctx)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if err := state.RequireCleanWorktree(ctx); err != nil {
		return domain.ContextCommit{}, err
	}
	contextStore, err := s.openStore(ctx, false)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	session, err := contextStore.MergeSession(ctx, sessionID)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if session.State != domain.MergeSessionReady || session.RepositoryID != key.RepositoryID || session.RefName != key.RefName || session.SourceSHA != key.SourceSHA {
		return domain.ContextCommit{}, domain.NewError(domain.CodeValidation, errors.New("merge session is not ready for the current context"))
	}
	ref, err := contextStore.ContextRef(ctx, key)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if ref.CommitID != session.LocalSnapshotID {
		return domain.ContextCommit{}, domain.NewError(domain.CodeConcurrentUpdate, errors.New("local context ref changed since merge start"))
	}
	pending, err := contextStore.PendingNotes(ctx, key)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if len(pending) != 0 {
		return domain.ContextCommit{}, domain.NewError(domain.CodeWorkingSetDirty, errors.New("commit or discard pending context changes before merging"))
	}
	local, err := contextStore.ReadContextObject(session.LocalSnapshotID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	remote, err := contextStore.ReadContextObject(session.RemoteSnapshotID, key.RepositoryID, key.RefName)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	if !domain.IsContextSnapshotSchema(local.SchemaVersion) || !domain.IsContextSnapshotSchema(remote.SchemaVersion) || local.SourceSHA != key.SourceSHA || remote.SourceSHA != key.SourceSHA || !sameContextSnapshotProvenance(local.Provenance, session.Provenance) || !sameContextSnapshotProvenance(remote.Provenance, session.Provenance) || !reflect.DeepEqual(local.Entities, remote.Entities) {
		return domain.ContextCommit{}, domain.NewError(domain.CodeValidation, errors.New("merge session snapshots no longer have compatible source, provenance, or entities"))
	}
	records, err := resolvedMergeRecords(session)
	if err != nil {
		return domain.ContextCommit{}, err
	}
	notes := make([]domain.Note, 0, len(records))
	for _, record := range records {
		note := record.Note
		note.Pending = false
		notes = append(notes, note)
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].ID < notes[j].ID })
	object := domain.ContextObject{SchemaVersion: contextSnapshotWriteVersion(local, remote), RepositoryID: key.RepositoryID, RefName: key.RefName, ParentIDs: []string{session.LocalSnapshotID, session.RemoteSnapshotID}, SourceSHA: key.SourceSHA, Message: session.Message, Author: session.Author, CreatedAt: session.PlannedCreatedAt, Provenance: session.Provenance, Entities: append([]domain.Entity(nil), local.Entities...), Notes: notes, RevisionMappings: contextRevisionMappings(notes)}
	sort.Slice(object.Entities, func(i, j int) bool { return object.Entities[i].Key < object.Entities[j].Key })
	contents, err := json.Marshal(object)
	if err != nil {
		return domain.ContextCommit{}, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("serialize merged context object: %w", err))
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
	commit := domain.ContextCommit{ID: identifier, ParentID: session.LocalSnapshotID, RefName: key.RefName, SourceSHA: key.SourceSHA, Message: session.Message, Author: session.Author, CreatedAt: session.PlannedCreatedAt}
	if err := contextStore.FinalizeMerge(ctx, store.FinalizeMergeInput{Key: key, SessionID: session.ID, Commit: commit, ParentIDs: object.ParentIDs, Notes: notes}); err != nil {
		return domain.ContextCommit{}, err
	}
	return commit, nil
}

func resolvedMergeRecords(session domain.MergeSession) ([]domain.SnapshotMergeRecord, error) {
	records := append([]domain.SnapshotMergeRecord(nil), session.AutomaticRecords...)
	for _, conflict := range session.Conflicts {
		var record *domain.SnapshotMergeRecord
		switch conflict.Resolution {
		case domain.MergeConflictLocal:
			record = conflict.Local
		case domain.MergeConflictRemote:
			record = conflict.Remote
		case domain.MergeConflictAuthored:
			record = conflict.Authored
		default:
			return nil, domain.NewError(domain.CodeValidation, errors.New("merge session has unresolved conflicts"))
		}
		if !validMergeRecord(conflict.NoteID, record, session.SourceSHA) {
			return nil, domain.NewError(domain.CodeValidation, errors.New("merge session has an invalid resolved record"))
		}
		records = append(records, *record)
	}
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		if !validMergeRecord(record.Note.ID, &record, session.SourceSHA) {
			return nil, domain.NewError(domain.CodeValidation, errors.New("merge session has an invalid automatic record"))
		}
		if _, found := seen[record.Note.ID]; found {
			return nil, domain.NewError(domain.CodeValidation, errors.New("merge session has duplicate resolved note IDs"))
		}
		seen[record.Note.ID] = struct{}{}
	}
	return records, nil
}

func validMergeRecord(noteID string, record *domain.SnapshotMergeRecord, sourceSHA string) bool {
	if record == nil || record.Note.ID != noteID || record.Note.RevisionID == "" || record.Note.EntityKey == "" || !domain.ValidNoteKind(record.Note.Kind) || strings.TrimSpace(record.Note.Body) == "" || record.Note.Author == "" || record.Note.Origin == "" || record.Note.CreatedAt.IsZero() || !domain.ValidNoteBindingState(record.Note.BindingState) || record.Note.BindingSourceSHA != sourceSHA {
		return false
	}
	mapping := record.Mapping
	return mapping.EntityKey == record.Note.EntityKey && mapping.NoteID == record.Note.ID && mapping.RevisionID == record.Note.RevisionID && mapping.BindingState == record.Note.BindingState && mapping.BindingSourceSHA == record.Note.BindingSourceSHA && mapping.ReviewReason == record.Note.ReviewReason
}

func authoredMergeRecord(session domain.MergeSession, input MergeResolveInput) (*domain.SnapshotMergeRecord, error) {
	conflictID := strings.TrimSpace(input.ConflictID)
	var noteID string
	var supersedesRevisionID string
	for _, conflict := range session.Conflicts {
		if conflict.ID == conflictID {
			noteID = conflict.NoteID
			if conflict.Base != nil {
				supersedesRevisionID = conflict.Base.Note.RevisionID
			}
			break
		}
	}
	if noteID == "" {
		return nil, domain.NewError(domain.CodeEntityNotFound, fmt.Errorf("merge conflict %q does not exist", conflictID))
	}
	entityKey := strings.TrimSpace(input.EntityKey)
	body := strings.TrimSpace(input.Body)
	author := strings.TrimSpace(input.Author)
	origin := strings.TrimSpace(input.Origin)
	kind := domain.NoteKind(strings.TrimSpace(input.Kind))
	if entityKey == "" || body == "" || author == "" || origin == "" || !domain.ValidNoteKind(kind) {
		return nil, domain.NewError(domain.CodeValidation, errors.New("authored merge resolution is incomplete or invalid"))
	}
	contents, err := json.Marshal(struct {
		LocalSnapshotID  string          `json:"local_snapshot_id"`
		RemoteSnapshotID string          `json:"remote_snapshot_id"`
		ConflictID       string          `json:"conflict_id"`
		NoteID           string          `json:"note_id"`
		Supersedes       string          `json:"supersedes_revision_id"`
		EntityKey        string          `json:"entity_key"`
		Kind             domain.NoteKind `json:"kind"`
		Body             string          `json:"body"`
		Author           string          `json:"author"`
		Origin           string          `json:"origin"`
	}{session.LocalSnapshotID, session.RemoteSnapshotID, conflictID, noteID, supersedesRevisionID, entityKey, kind, body, author, origin})
	if err != nil {
		return nil, domain.NewError(domain.CodeLocalStorage, fmt.Errorf("serialize authored merge resolution: %w", err))
	}
	digest := blake3.Sum256(contents)
	revisionID := fmt.Sprintf("%x", digest[:])
	note := domain.Note{ID: noteID, RevisionID: revisionID, SupersedesRevisionID: supersedesRevisionID, EntityKey: entityKey, Kind: kind, Body: body, Author: author, Origin: origin, CreatedAt: session.PlannedCreatedAt, BindingState: domain.NoteBindingActive, BindingSourceSHA: session.SourceSHA}
	return &domain.SnapshotMergeRecord{Note: note, Mapping: domain.ContextRevisionMapping{EntityKey: note.EntityKey, NoteID: note.ID, RevisionID: note.RevisionID, BindingState: note.BindingState, BindingSourceSHA: note.BindingSourceSHA}}, nil
}
