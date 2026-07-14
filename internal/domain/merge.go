package domain

import (
	"errors"
	"reflect"
	"sort"
	"time"
)

type SnapshotMergeInput struct {
	Base   ContextObject
	Local  ContextObject
	Remote ContextObject
}

type SnapshotMergeRecord struct {
	Note    Note                   `json:"note"`
	Mapping ContextRevisionMapping `json:"mapping"`
}

type SnapshotMergeConflict struct {
	NoteID string               `json:"note_id"`
	Base   *SnapshotMergeRecord `json:"base,omitempty"`
	Local  *SnapshotMergeRecord `json:"local,omitempty"`
	Remote *SnapshotMergeRecord `json:"remote,omitempty"`
}

type SnapshotMergePlan struct {
	Records   []SnapshotMergeRecord   `json:"records"`
	Conflicts []SnapshotMergeConflict `json:"conflicts"`
}

type MergeSessionState string

const (
	MergeSessionOpen      MergeSessionState = "open"
	MergeSessionReady     MergeSessionState = "ready"
	MergeSessionCommitted MergeSessionState = "committed"
)

type MergeConflictResolution string

const (
	MergeConflictUnresolved MergeConflictResolution = "unresolved"
	MergeConflictLocal      MergeConflictResolution = "local"
	MergeConflictRemote     MergeConflictResolution = "remote"
	MergeConflictAuthored   MergeConflictResolution = "authored"
)

type MergeSessionConflict struct {
	ID string `json:"id"`
	SnapshotMergeConflict
	Resolution MergeConflictResolution `json:"resolution"`
	Authored   *SnapshotMergeRecord    `json:"authored,omitempty"`
}

type MergeSession struct {
	ID               string                      `json:"id"`
	LocalSnapshotID  string                      `json:"local_snapshot_id"`
	RemoteSnapshotID string                      `json:"remote_snapshot_id"`
	BaseSnapshotID   string                      `json:"base_snapshot_id"`
	RepositoryID     string                      `json:"repository_id"`
	RefName          string                      `json:"ref_name"`
	SourceSHA        string                      `json:"source_sha"`
	Provenance       []ContextSnapshotProvenance `json:"provenance"`
	Message          string                      `json:"message"`
	Author           string                      `json:"author"`
	PlannedCreatedAt time.Time                   `json:"planned_created_at"`
	State            MergeSessionState           `json:"state"`
	AutomaticRecords []SnapshotMergeRecord       `json:"automatic_records"`
	Conflicts        []MergeSessionConflict      `json:"conflicts"`
}

func PlanSnapshotMerge(input SnapshotMergeInput) (SnapshotMergePlan, error) {
	if err := validateMergeSnapshots(input); err != nil {
		return SnapshotMergePlan{}, err
	}
	base, err := snapshotMergeRecords(input.Base)
	if err != nil {
		return SnapshotMergePlan{}, err
	}
	local, err := snapshotMergeRecords(input.Local)
	if err != nil {
		return SnapshotMergePlan{}, err
	}
	remote, err := snapshotMergeRecords(input.Remote)
	if err != nil {
		return SnapshotMergePlan{}, err
	}
	identifiers := make(map[string]struct{}, len(base)+len(local)+len(remote))
	for _, records := range []map[string]SnapshotMergeRecord{base, local, remote} {
		for identifier := range records {
			identifiers[identifier] = struct{}{}
		}
	}
	keys := make([]string, 0, len(identifiers))
	for identifier := range identifiers {
		keys = append(keys, identifier)
	}
	sort.Strings(keys)
	plan := SnapshotMergePlan{}
	for _, identifier := range keys {
		baseRecord, baseFound := base[identifier]
		localRecord, localFound := local[identifier]
		remoteRecord, remoteFound := remote[identifier]
		switch {
		case mergeRecordsEqual(localRecord, localFound, remoteRecord, remoteFound):
			if localFound {
				plan.Records = append(plan.Records, localRecord)
			}
		case mergeRecordsEqual(localRecord, localFound, baseRecord, baseFound):
			if remoteFound {
				plan.Records = append(plan.Records, remoteRecord)
			}
		case mergeRecordsEqual(remoteRecord, remoteFound, baseRecord, baseFound):
			if localFound {
				plan.Records = append(plan.Records, localRecord)
			}
		default:
			plan.Conflicts = append(plan.Conflicts, SnapshotMergeConflict{NoteID: identifier, Base: mergeRecordPointer(baseRecord, baseFound), Local: mergeRecordPointer(localRecord, localFound), Remote: mergeRecordPointer(remoteRecord, remoteFound)})
		}
	}
	return plan, nil
}

func validateMergeSnapshots(input SnapshotMergeInput) error {
	if !IsContextSnapshotSchema(input.Base.SchemaVersion) || !IsContextSnapshotSchema(input.Local.SchemaVersion) || !IsContextSnapshotSchema(input.Remote.SchemaVersion) {
		return NewError(CodeValidation, errors.New("snapshot merge requires schema-v3 or schema-v4 inputs"))
	}
	if input.Base.RepositoryID != input.Local.RepositoryID || input.Base.RepositoryID != input.Remote.RepositoryID || input.Base.RefName != input.Local.RefName || input.Base.RefName != input.Remote.RefName || input.Base.SourceSHA != input.Local.SourceSHA || input.Base.SourceSHA != input.Remote.SourceSHA || !sameSnapshotProvenance(input.Base.Provenance, input.Local.Provenance) || !sameSnapshotProvenance(input.Base.Provenance, input.Remote.Provenance) {
		return NewError(CodeValidation, errors.New("snapshot merge inputs must share repository, ref, source, and provenance"))
	}
	return nil
}

func snapshotMergeRecords(snapshot ContextObject) (map[string]SnapshotMergeRecord, error) {
	if len(snapshot.Notes) != len(snapshot.RevisionMappings) {
		return nil, NewError(CodeValidation, errors.New("snapshot merge input has incomplete revision mappings"))
	}
	mappings := make(map[string]ContextRevisionMapping, len(snapshot.RevisionMappings))
	for _, mapping := range snapshot.RevisionMappings {
		if mapping.NoteID == "" || mapping.RevisionID == "" {
			return nil, NewError(CodeValidation, errors.New("snapshot merge input has invalid revision mapping"))
		}
		if _, found := mappings[mapping.NoteID]; found {
			return nil, NewError(CodeValidation, errors.New("snapshot merge input has duplicate mapped note IDs"))
		}
		mappings[mapping.NoteID] = mapping
	}
	records := make(map[string]SnapshotMergeRecord, len(snapshot.Notes))
	for _, note := range snapshot.Notes {
		mapping, found := mappings[note.ID]
		if !found || mapping.RevisionID != note.RevisionID || mapping.EntityKey != note.EntityKey || mapping.BindingState != note.BindingState || mapping.BindingSourceSHA != note.BindingSourceSHA || mapping.ReviewReason != note.ReviewReason {
			return nil, NewError(CodeValidation, errors.New("snapshot merge input has mismatched note mapping"))
		}
		if _, found := records[note.ID]; found {
			return nil, NewError(CodeValidation, errors.New("snapshot merge input has duplicate note IDs"))
		}
		records[note.ID] = SnapshotMergeRecord{Note: note, Mapping: mapping}
	}
	return records, nil
}

func sameSnapshotProvenance(left, right []ContextSnapshotProvenance) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func mergeRecordsEqual(left SnapshotMergeRecord, leftFound bool, right SnapshotMergeRecord, rightFound bool) bool {
	return leftFound == rightFound && (!leftFound || reflect.DeepEqual(left, right))
}

func mergeRecordPointer(record SnapshotMergeRecord, found bool) *SnapshotMergeRecord {
	if !found {
		return nil
	}
	return &record
}
