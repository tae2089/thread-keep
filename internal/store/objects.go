package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/zeebo/blake3"
)

type storedContextObject struct {
	ID       string
	Contents []byte
	Object   domain.ContextObject
}

type RemoteObjectReader interface {
	ReadObject(context.Context, string) ([]byte, error)
}

type ObjectChain struct {
	TipID     string
	SourceSHA string
	Count     int
}

type ObjectRecord struct {
	ID        string
	Contents  []byte
	SourceSHA string
}

func ReadContextObject(layout Layout, commitID, repositoryID, refName string) (domain.ContextObject, error) {
	graph, err := (&Store{layout: layout}).loadObjectGraph(commitID, repositoryID, refName)
	if err != nil {
		return domain.ContextObject{}, err
	}
	return graph[len(graph)-1].Object, nil
}

func (s *Store) WriteObject(id string, contents []byte) error {
	path := filepath.Join(s.layout.ObjectDir, id+".json")
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, contents) {
			return nil
		}
		return domain.NewError(domain.CodeLocalStorage, fmt.Errorf("context object %s already exists with different contents", id))
	} else if !errors.Is(err, os.ErrNotExist) {
		return localError("read context object", err)
	}
	temporary, err := os.CreateTemp(s.layout.ObjectDir, ".context-*.tmp")
	if err != nil {
		return localError("create context object temp file", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return localError("write context object", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return localError("sync context object", err)
	}
	if err := temporary.Close(); err != nil {
		return localError("close context object", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, contents) {
			return nil
		}
		return localError("publish context object", err)
	}
	directory, err := os.Open(s.layout.ObjectDir)
	if err == nil {
		defer directory.Close()
		if err := directory.Sync(); err != nil {
			return localError("sync context object directory", err)
		}
	}
	return nil
}

func (s *Store) ReadObjectChain(commitID, repositoryID, refName string) ([]ObjectRecord, error) {
	chain, err := s.loadObjectGraph(commitID, repositoryID, refName)
	if err != nil {
		return nil, err
	}
	objects := make([]ObjectRecord, 0, len(chain))
	for _, item := range chain {
		objects = append(objects, ObjectRecord{ID: item.ID, Contents: append([]byte(nil), item.Contents...), SourceSHA: item.Object.SourceSHA})
	}
	return objects, nil
}

func (s *Store) ReadContextObject(commitID, repositoryID, refName string) (domain.ContextObject, error) {
	chain, err := s.loadObjectGraph(commitID, repositoryID, refName)
	if err != nil {
		return domain.ContextObject{}, err
	}
	return chain[len(chain)-1].Object, nil
}

func (s *Store) ImportObjectChain(ctx context.Context, commitID, repositoryID, refName string, reader RemoteObjectReader) (ObjectChain, error) {
	if reader == nil {
		return ObjectChain{}, domain.NewError(domain.CodeValidation, errors.New("remote object reader is not configured"))
	}
	chain, err := loadObjectGraphFrom(commitID, repositoryID, refName, func(id string) ([]byte, error) {
		return reader.ReadObject(ctx, id)
	})
	if err != nil {
		return ObjectChain{}, err
	}
	for _, item := range chain {
		if err := s.WriteObject(item.ID, item.Contents); err != nil {
			return ObjectChain{}, err
		}
	}
	tip := chain[len(chain)-1]
	return ObjectChain{TipID: tip.ID, SourceSHA: tip.Object.SourceSHA, Count: len(chain)}, nil
}

func (s *Store) IsAncestor(descendantID, ancestorID, repositoryID, refName string) (bool, error) {
	if ancestorID == "" {
		return true, nil
	}
	ancestorID, err := normalizeObjectID(ancestorID)
	if err != nil {
		return false, err
	}
	chain, err := s.loadObjectGraph(descendantID, repositoryID, refName)
	if err != nil {
		return false, err
	}
	for _, item := range chain {
		if item.ID == ancestorID {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) FindMergeBase(localID, remoteID, repositoryID, refName string) (string, error) {
	localID, err := normalizeObjectID(localID)
	if err != nil {
		return "", err
	}
	remoteID, err = normalizeObjectID(remoteID)
	if err != nil {
		return "", err
	}
	localGraph, err := s.loadObjectGraph(localID, repositoryID, refName)
	if err != nil {
		return "", err
	}
	remoteGraph, err := s.loadObjectGraph(remoteID, repositoryID, refName)
	if err != nil {
		return "", err
	}
	localDistances, err := snapshotAncestorDistances(localID, localGraph)
	if err != nil {
		return "", err
	}
	remoteDistances, err := snapshotAncestorDistances(remoteID, remoteGraph)
	if err != nil {
		return "", err
	}
	baseID := ""
	bestMaximum, bestTotal := 0, 0
	for identifier, localDistance := range localDistances {
		remoteDistance, found := remoteDistances[identifier]
		if !found {
			continue
		}
		maximum := localDistance
		if remoteDistance > maximum {
			maximum = remoteDistance
		}
		total := localDistance + remoteDistance
		if baseID == "" || maximum < bestMaximum || (maximum == bestMaximum && (total < bestTotal || (total == bestTotal && identifier < baseID))) {
			baseID, bestMaximum, bestTotal = identifier, maximum, total
		}
	}
	if baseID == "" {
		return "", domain.NewError(domain.CodeRemoteConflict, errors.New("context snapshots have no common merge base"))
	}
	return baseID, nil
}

func snapshotAncestorDistances(tipID string, graph []storedContextObject) (map[string]int, error) {
	objects := make(map[string]domain.ContextObject, len(graph))
	for _, item := range graph {
		objects[item.ID] = item.Object
	}
	if _, found := objects[tipID]; !found {
		return nil, domain.NewError(domain.CodeValidation, errors.New("snapshot graph does not contain its tip"))
	}
	distances := map[string]int{tipID: 0}
	queue := []string{tipID}
	for len(queue) != 0 {
		identifier := queue[0]
		queue = queue[1:]
		distance := distances[identifier]
		object := objects[identifier]
		for _, parentID := range contextObjectParentIDs(object) {
			if _, found := objects[parentID]; !found {
				return nil, domain.NewError(domain.CodeValidation, errors.New("snapshot graph is missing a parent object"))
			}
			nextDistance := distance + 1
			previousDistance, found := distances[parentID]
			if found && previousDistance <= nextDistance {
				continue
			}
			distances[parentID] = nextDistance
			queue = append(queue, parentID)
		}
	}
	return distances, nil
}

func projectionEmpty(ctx context.Context, conn *sql.Conn) (bool, error) {
	for _, table := range []string{"working_sets", "entities", "language_coverage", "pending_notes", "context_commits", "context_refs", "committed_notes", "search_index"} {
		var exists bool
		if err := conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" LIMIT 1)").Scan(&exists); err != nil {
			return false, localError("inspect existing projection", err)
		}
		if exists {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) Rebuild(ctx context.Context, input RebuildInput) (string, int, error) {
	chain, err := s.loadObjectGraph(input.CommitID, input.Key.RepositoryID, input.Key.RefName)
	if err != nil {
		return "", 0, err
	}
	tipID := chain[len(chain)-1].ID
	err = s.withImmediate(ctx, func(conn *sql.Conn) error {
		empty, err := projectionEmpty(ctx, conn)
		if err != nil {
			return err
		}
		if !empty {
			return domain.NewError(domain.CodeWorkingSetDirty, errors.New("local context projection is not empty"))
		}
		for _, item := range chain {
			object := item.Object
			parentID := contextObjectPrimaryParentID(object)
			if _, err := conn.ExecContext(ctx, `INSERT INTO context_commits (commit_id, parent_id, ref_name, source_sha, message, author, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, item.ID, parentID, object.RefName, object.SourceSHA, object.Message, object.Author, object.CreatedAt.UnixNano()); err != nil {
				return localError("restore context commit", err)
			}
			if err := insertContextCommitParents(ctx, conn, item.ID, contextObjectParentIDs(object)); err != nil {
				return err
			}
			for _, note := range object.Notes {
				topics, err := encodeNoteTopics(note.Topics)
				if err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx, `INSERT INTO committed_notes (context_commit_id, note_id, revision_id, supersedes_revision_id, entity_key, kind, body, author, origin, created_at, binding_state, binding_source_sha, review_reason, topics_json)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item.ID, note.ID, note.RevisionID, note.SupersedesRevisionID, note.EntityKey, note.Kind, note.Body, note.Author, note.Origin, note.CreatedAt.UnixNano(), note.BindingState, note.BindingSourceSHA, note.ReviewReason, topics); err != nil {
					return localError("restore committed note", err)
				}
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO context_refs (ref_name, commit_id, source_sha, version) VALUES (?, ?, ?, ?)", input.Key.RefName, tipID, chain[len(chain)-1].Object.SourceSHA, len(chain)); err != nil {
			return localError("restore context ref", err)
		}
		return s.applyIndexUpdate(ctx, conn, input.Key, input.Projections, false)
	})
	if err != nil {
		return "", 0, err
	}
	return tipID, len(chain), nil
}

func (s *Store) loadObjectGraph(commitID, repositoryID, refName string) ([]storedContextObject, error) {
	return loadObjectGraphFrom(commitID, repositoryID, refName, func(id string) ([]byte, error) {
		contents, err := os.ReadFile(filepath.Join(s.layout.ObjectDir, id+".json"))
		if err != nil {
			return nil, localError("read context object", err)
		}
		return contents, nil
	})
}

func loadObjectGraphFrom(commitID, repositoryID, refName string, readObject func(string) ([]byte, error)) ([]storedContextObject, error) {
	rootID, err := normalizeObjectID(commitID)
	if err != nil {
		return nil, err
	}
	type visitState int
	const (
		unvisited visitState = iota
		visiting
		visited
	)
	states := make(map[string]visitState)
	var graph []storedContextObject
	var visit func(string) error
	visit = func(id string) error {
		switch states[id] {
		case visiting:
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot ancestry contains a cycle"))
		case visited:
			return nil
		}
		states[id] = visiting
		contents, err := readObject(id)
		if err != nil {
			return err
		}
		digest := blake3.Sum256(contents)
		if fmt.Sprintf("%x", digest[:]) != id {
			return domain.NewError(domain.CodeValidation, fmt.Errorf("context object %s does not match its content ID", id))
		}
		object, err := decodeContextObject(contents)
		if err != nil {
			return err
		}
		if object.SchemaVersion == 1 {
			for index := range object.Notes {
				object.Notes[index] = domain.NormalizeLegacyNote(object.Notes[index], object.SourceSHA)
			}
		}
		if err := validateContextObject(object, repositoryID, refName); err != nil {
			return err
		}
		for _, parentID := range contextObjectParentIDs(object) {
			parentID, err := normalizeObjectID(parentID)
			if err != nil {
				return err
			}
			if err := visit(parentID); err != nil {
				return err
			}
		}
		states[id] = visited
		graph = append(graph, storedContextObject{ID: id, Contents: contents, Object: object})
		return nil
	}
	if err := visit(rootID); err != nil {
		return nil, err
	}
	if err := validateRevisionLinks(graph); err != nil {
		return nil, err
	}
	if err := validateLandingReceiptLinks(graph); err != nil {
		return nil, err
	}
	return graph, nil
}

func decodeContextObject(contents []byte) (domain.ContextObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var object domain.ContextObject
	if err := decoder.Decode(&object); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return domain.ContextObject{}, domain.NewError(domain.CodeValidation, fmt.Errorf("decode context object: %w", err))
		}
		return domain.ContextObject{}, localError("decode context object", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return domain.ContextObject{}, domain.NewError(domain.CodeValidation, errors.New("context object must contain one JSON value"))
	}
	return object, nil
}

func validateRevisionLinks(chain []storedContextObject) error {
	knownRevisions := make(map[string]string)
	revisionContents := make(map[string]domain.Note)
	for _, item := range chain {
		for _, note := range item.Object.Notes {
			if note.SupersedesRevisionID != "" {
				predecessorNoteID, found := knownRevisions[note.SupersedesRevisionID]
				if !found {
					return domain.NewError(domain.CodeValidation, errors.New("context object supersedes an unknown note revision"))
				}
				if predecessorNoteID != note.ID {
					return domain.NewError(domain.CodeValidation, errors.New("context object supersedes a revision from a different note"))
				}
			}
			immutable := immutableRevisionContent(note)
			if previous, found := revisionContents[note.RevisionID]; found && !reflect.DeepEqual(previous, immutable) {
				return domain.NewError(domain.CodeValidation, errors.New("context object changes immutable content for an existing note revision"))
			}
			revisionContents[note.RevisionID] = immutable
			knownRevisions[note.RevisionID] = note.ID
		}
	}
	return nil
}

func validateLandingReceiptLinks(chain []storedContextObject) error {
	objectIDs := make(map[string]struct{}, len(chain))
	for _, item := range chain {
		objectIDs[item.ID] = struct{}{}
	}
	seen := make(map[string]struct{})
	for _, item := range chain {
		for _, receipt := range item.Object.LandingReceipts {
			if _, found := seen[receipt.ID]; found {
				return domain.NewError(domain.CodeValidation, errors.New("context object ancestry contains duplicate landing receipt IDs"))
			}
			seen[receipt.ID] = struct{}{}
			if receipt.BaseContextCommitID != "" {
				if _, found := objectIDs[receipt.BaseContextCommitID]; !found {
					return domain.NewError(domain.CodeValidation, errors.New("landing receipt base context is not in object ancestry"))
				}
			}
		}
	}
	return nil
}

func immutableRevisionContent(note domain.Note) domain.Note {
	note.EntityKey = ""
	note.BindingState = ""
	note.BindingSourceSHA = ""
	note.ReviewReason = ""
	note.Pending = false
	return note
}

func normalizeObjectID(value string) (string, error) {
	return domain.NormalizeContextCommitID(value)
}

func validateContextObject(object domain.ContextObject, repositoryID, refName string) error {
	if object.SchemaVersion != 1 && object.SchemaVersion != 2 && !domain.IsContextSnapshotSchema(object.SchemaVersion) {
		return domain.NewError(domain.CodeValidation, fmt.Errorf("unsupported context object schema version %d", object.SchemaVersion))
	}
	if object.RepositoryID != repositoryID {
		return domain.NewError(domain.CodeValidation, errors.New("context object belongs to a different repository"))
	}
	if object.RefName != refName {
		return domain.NewError(domain.CodeValidation, errors.New("context object belongs to a different context ref"))
	}
	if strings.TrimSpace(object.SourceSHA) == "" || strings.TrimSpace(object.Message) == "" || strings.TrimSpace(object.Author) == "" || object.CreatedAt.IsZero() {
		return domain.NewError(domain.CodeValidation, errors.New("context object commit metadata is incomplete"))
	}
	entitiesByKey := make(map[string]domain.Entity, len(object.Entities))
	for _, entity := range object.Entities {
		if entity.Language == "" || entity.Key == "" || entity.Path == "" || entity.SourceSHA != object.SourceSHA {
			return domain.NewError(domain.CodeValidation, errors.New("context object contains an invalid entity"))
		}
		if _, found := entitiesByKey[entity.Key]; found {
			return domain.NewError(domain.CodeValidation, errors.New("context object contains duplicate entity keys"))
		}
		entitiesByKey[entity.Key] = entity
	}
	noteIDs := make(map[string]struct{}, len(object.Notes))
	for _, note := range object.Notes {
		topics, err := domain.NormalizeNoteTopics(note.Topics)
		if err != nil || !slices.Equal(topics, note.Topics) {
			return domain.NewError(domain.CodeValidation, errors.New("context object contains invalid note topics"))
		}
		if note.ID == "" || note.EntityKey == "" || !domain.ValidNoteKind(note.Kind) || strings.TrimSpace(note.Body) == "" || note.Author == "" || note.Origin == "" || note.CreatedAt.IsZero() || note.Pending {
			return domain.NewError(domain.CodeValidation, errors.New("context object contains an invalid committed note"))
		}
		if object.SchemaVersion >= 2 && (note.RevisionID == "" || !domain.ValidNoteBindingState(note.BindingState) || note.BindingSourceSHA != object.SourceSHA) {
			return domain.NewError(domain.CodeValidation, errors.New("context object contains an invalid note revision or binding"))
		}
		if _, found := noteIDs[note.ID]; found {
			return domain.NewError(domain.CodeValidation, errors.New("context object contains duplicate note IDs"))
		}
		noteIDs[note.ID] = struct{}{}
	}
	if domain.IsContextSnapshotSchema(object.SchemaVersion) {
		if err := validateContextSnapshotManifest(object); err != nil {
			return err
		}
	}
	if object.SchemaVersion < 4 && len(object.LandingReceipts) != 0 {
		return domain.NewError(domain.CodeValidation, errors.New("landing receipts require context object schema version 4"))
	}
	if object.SchemaVersion == 4 {
		if err := validateLandingReceipts(object); err != nil {
			return err
		}
	}
	return nil
}

// ValidateContextObject validates a decoded immutable context object for a repository/ref.
func ValidateContextObject(object domain.ContextObject, repositoryID, refName string) error {
	return validateContextObject(object, repositoryID, refName)
}

func contextObjectParentIDs(object domain.ContextObject) []string {
	if !domain.IsContextSnapshotSchema(object.SchemaVersion) {
		if object.ParentID == "" {
			return nil
		}
		return []string{object.ParentID}
	}
	if len(object.ParentIDs) != 0 {
		return append([]string(nil), object.ParentIDs...)
	}
	if object.LegacyParentID == "" {
		return nil
	}
	return []string{object.LegacyParentID}
}

func validateLandingReceipts(object domain.ContextObject) error {
	previousID := ""
	for _, receipt := range object.LandingReceipts {
		if receipt.ID == "" || receipt.Provider == "" || receipt.ForgeRepository == "" || receipt.ChangeNumber < 1 || receipt.ContextRepositoryID != object.RepositoryID || receipt.TargetRef != object.RefName || receipt.FinalPlanID == "" || receipt.SourceMergeSHA != object.SourceSHA || receipt.Resolver == "" {
			return domain.NewError(domain.CodeValidation, errors.New("context object contains an invalid landing receipt"))
		}
		for _, digest := range []string{receipt.ID, receipt.FinalPlanID, receipt.CandidateDigest, receipt.BaseContextCommitID} {
			if digest == "" {
				continue
			}
			normalized, err := normalizeObjectID(digest)
			if err != nil || normalized != digest {
				return domain.NewError(domain.CodeValidation, errors.New("landing receipt contains an invalid content identifier"))
			}
		}
		if previousID != "" && receipt.ID <= previousID {
			return domain.NewError(domain.CodeValidation, errors.New("landing receipts are not canonical or contain duplicate IDs"))
		}
		previousID = receipt.ID
		previousMapping := ""
		for _, mapping := range receipt.CandidateMappings {
			key := mapping.CandidateRecordID + "\x00" + mapping.NoteID + "\x00" + mapping.RevisionID
			if mapping.CandidateRecordID == "" || mapping.NoteID == "" || mapping.RevisionID == "" || (previousMapping != "" && key <= previousMapping) {
				return domain.NewError(domain.CodeValidation, errors.New("landing receipt candidate mappings are invalid or not canonical"))
			}
			previousMapping = key
		}
	}
	return nil
}

func contextObjectPrimaryParentID(object domain.ContextObject) string {
	parents := contextObjectParentIDs(object)
	if len(parents) == 0 {
		return ""
	}
	return parents[0]
}

func validateContextSnapshotManifest(object domain.ContextObject) error {
	if object.ParentID != "" || len(object.ParentIDs) > 2 || (object.LegacyParentID != "" && len(object.ParentIDs) != 0) {
		return domain.NewError(domain.CodeValidation, errors.New("context snapshot parent shape is invalid"))
	}
	if object.LegacyParentID != "" {
		normalized, err := normalizeObjectID(object.LegacyParentID)
		if err != nil || normalized != object.LegacyParentID {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot legacy parent ID is invalid"))
		}
	}
	parents := make(map[string]struct{}, len(object.ParentIDs))
	for _, parentID := range object.ParentIDs {
		normalized, err := normalizeObjectID(parentID)
		if err != nil || normalized != parentID {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot parent ID is invalid"))
		}
		if _, found := parents[parentID]; found {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot contains duplicate parent IDs"))
		}
		parents[parentID] = struct{}{}
	}
	if len(object.Provenance) == 0 {
		return domain.NewError(domain.CodeValidation, errors.New("context snapshot provenance is required"))
	}
	provenanceLanguages := make(map[string]struct{}, len(object.Provenance))
	previousLanguage := ""
	for _, provenance := range object.Provenance {
		if provenance.Language == "" || provenance.IndexerID == "" || provenance.IndexerVersion == "" || provenance.SourceSHA != object.SourceSHA || (previousLanguage != "" && provenance.Language <= previousLanguage) {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot provenance is invalid or not canonical"))
		}
		if _, found := provenanceLanguages[provenance.Language]; found {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot contains duplicate provenance languages"))
		}
		provenanceLanguages[provenance.Language] = struct{}{}
		previousLanguage = provenance.Language
	}
	for _, entity := range object.Entities {
		if _, found := provenanceLanguages[entity.Language]; !found {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot entity is missing language provenance"))
		}
	}
	if len(object.RevisionMappings) != len(object.Notes) {
		return domain.NewError(domain.CodeValidation, errors.New("context snapshot mappings must cover every note"))
	}
	notes := make(map[string]domain.Note, len(object.Notes))
	for _, note := range object.Notes {
		notes[note.ID] = note
	}
	mappingNotes := make(map[string]struct{}, len(object.RevisionMappings))
	mappingRevisions := make(map[string]struct{}, len(object.RevisionMappings))
	previousMapping := ""
	for _, mapping := range object.RevisionMappings {
		mappingKey := mapping.EntityKey + "\x00" + mapping.NoteID + "\x00" + mapping.RevisionID
		if mapping.EntityKey == "" || mapping.NoteID == "" || mapping.RevisionID == "" || !domain.ValidNoteBindingState(mapping.BindingState) || mapping.BindingSourceSHA != object.SourceSHA || (previousMapping != "" && mappingKey <= previousMapping) {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot revision mapping is invalid or not canonical"))
		}
		note, found := notes[mapping.NoteID]
		if !found || note.EntityKey != mapping.EntityKey || note.RevisionID != mapping.RevisionID || note.BindingState != mapping.BindingState || note.BindingSourceSHA != mapping.BindingSourceSHA || note.ReviewReason != mapping.ReviewReason {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot revision mapping does not match its note"))
		}
		if _, found := mappingNotes[mapping.NoteID]; found {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot contains duplicate mapped note IDs"))
		}
		if _, found := mappingRevisions[mapping.RevisionID]; found {
			return domain.NewError(domain.CodeValidation, errors.New("context snapshot contains duplicate mapped revision IDs"))
		}
		mappingNotes[mapping.NoteID] = struct{}{}
		mappingRevisions[mapping.RevisionID] = struct{}{}
		previousMapping = mappingKey
	}
	return nil
}
