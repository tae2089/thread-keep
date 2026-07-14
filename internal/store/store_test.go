package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tae2089/thread-keep/internal/domain"
	"github.com/zeebo/blake3"
)

func TestRebuildRejectsInvalidObjectsWithoutPartialProjection(t *testing.T) {
	tests := []struct {
		name     string
		prepare  func(*testing.T, *Store, domain.ContextObject) string
		wantCode domain.ErrorCode
	}{
		{name: "malformed ID", prepare: func(_ *testing.T, _ *Store, _ domain.ContextObject) string { return "not-an-id" }, wantCode: domain.CodeValidation},
		{name: "missing object", prepare: func(_ *testing.T, _ *Store, _ domain.ContextObject) string { return strings.Repeat("a", 64) }, wantCode: domain.CodeLocalStorage},
		{name: "malformed JSON", prepare: func(t *testing.T, contextStore *Store, _ domain.ContextObject) string {
			contents := []byte("{")
			return writeRawContextObject(t, contextStore, contents)
		}, wantCode: domain.CodeLocalStorage},
		{name: "hash mismatch", prepare: func(t *testing.T, contextStore *Store, object domain.ContextObject) string {
			identifier := writeTestContextObject(t, contextStore, object)
			if err := os.WriteFile(filepath.Join(contextStore.layout.ObjectDir, identifier+".json"), []byte("changed"), 0o644); err != nil {
				t.Fatalf("tamper object: %v", err)
			}
			return identifier
		}, wantCode: domain.CodeValidation},
		{name: "unsupported schema", prepare: func(t *testing.T, contextStore *Store, object domain.ContextObject) string {
			object.SchemaVersion = 3
			return writeTestContextObject(t, contextStore, object)
		}, wantCode: domain.CodeValidation},
		{name: "missing source revision", prepare: func(t *testing.T, contextStore *Store, object domain.ContextObject) string {
			object.SourceSHA = ""
			return writeTestContextObject(t, contextStore, object)
		}, wantCode: domain.CodeValidation},
		{name: "different repository", prepare: func(t *testing.T, contextStore *Store, object domain.ContextObject) string {
			object.RepositoryID = "other"
			return writeTestContextObject(t, contextStore, object)
		}, wantCode: domain.CodeValidation},
		{name: "different ref", prepare: func(t *testing.T, contextStore *Store, object domain.ContextObject) string {
			object.RefName = "refs/contexts/heads/other"
			return writeTestContextObject(t, contextStore, object)
		}, wantCode: domain.CodeValidation},
		{name: "duplicate note IDs", prepare: func(t *testing.T, contextStore *Store, object domain.ContextObject) string {
			note := domain.Note{ID: "duplicate", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "note", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC()}
			object.Notes = []domain.Note{note, note}
			return writeTestContextObject(t, contextStore, object)
		}, wantCode: domain.CodeValidation},
		{name: "unknown superseded revision", prepare: func(t *testing.T, contextStore *Store, object domain.ContextObject) string {
			object.SchemaVersion = 2
			object.Notes = []domain.Note{{ID: "note", RevisionID: "revision", SupersedesRevisionID: "missing", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "note", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: object.SourceSHA}}
			return writeTestContextObject(t, contextStore, object)
		}, wantCode: domain.CodeValidation},
		{name: "cross note superseded revision", prepare: func(t *testing.T, contextStore *Store, object domain.ContextObject) string {
			object.SchemaVersion = 2
			object.Notes = []domain.Note{
				{ID: "first", RevisionID: "first-revision", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "first", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: object.SourceSHA},
				{ID: "second", RevisionID: "second-revision", SupersedesRevisionID: "first-revision", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "second", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: object.SourceSHA},
			}
			return writeTestContextObject(t, contextStore, object)
		}, wantCode: domain.CodeValidation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			contextStore, err := Open(ctx, NewLayout(t.TempDir()))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer contextStore.Close()
			key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/heads/main", SourceSHA: "source"}
			object := domain.ContextObject{SchemaVersion: 1, RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: "old-source", Message: "context", Author: "tester", CreatedAt: time.Now().UTC()}
			identifier := test.prepare(t, contextStore, object)
			_, _, err = contextStore.Rebuild(ctx, RebuildInput{Key: key, CommitID: identifier, Projections: testProjection(key)})
			if domain.CodeOf(err) != test.wantCode {
				t.Fatalf("Rebuild() error = %v, want %s", err, test.wantCode)
			}
			assertEmptyProjection(t, contextStore, key)
		})
	}
}

func TestRebuildRollsBackWriteFailureAndRejectsExistingProjection(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/heads/main", SourceSHA: "source"}
	note := domain.Note{ID: "note", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "note", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC()}
	object := domain.ContextObject{SchemaVersion: 1, RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: "old-source", Message: "context", Author: "tester", CreatedAt: time.Now().UTC(), Notes: []domain.Note{note}}
	identifier := writeTestContextObject(t, contextStore, object)
	if _, err := contextStore.db.ExecContext(ctx, `CREATE TRIGGER fail_restored_note BEFORE INSERT ON committed_notes BEGIN SELECT RAISE(ABORT, 'forced restore failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	if _, _, err := contextStore.Rebuild(ctx, RebuildInput{Key: key, CommitID: identifier, Projections: testProjection(key)}); domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("Rebuild() forced write error = %v, want local storage", err)
	}
	assertEmptyProjection(t, contextStore, key)
	if _, err := contextStore.db.ExecContext(ctx, "DROP TRIGGER fail_restored_note"); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}

	if err := contextStore.ApplyIndexUpdate(ctx, key, testProjection(key)); err != nil {
		t.Fatalf("ApplyIndexUpdate() error = %v", err)
	}
	before, err := contextStore.Status(ctx, key)
	if err != nil {
		t.Fatalf("Status(before) error = %v", err)
	}
	valid := writeTestContextObject(t, contextStore, domain.ContextObject{SchemaVersion: 1, RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: "old-source", Message: "context", Author: "tester", CreatedAt: time.Now().UTC()})
	if _, _, err := contextStore.Rebuild(ctx, RebuildInput{Key: key, CommitID: valid, Projections: testProjection(key)}); domain.CodeOf(err) != domain.CodeWorkingSetDirty {
		t.Fatalf("Rebuild() existing projection error = %v, want working set dirty", err)
	}
	after, err := contextStore.Status(ctx, key)
	if err != nil {
		t.Fatalf("Status(after) error = %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("status changed after rejected rebuild: before=%+v after=%+v", before, after)
	}
}

func TestLoadObjectGraphNormalizesLegacyNoteRevisionAndBinding(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	legacy := domain.Note{ID: "note", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "keep this", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC()}
	object := domain.ContextObject{SchemaVersion: 1, RepositoryID: "repo", RefName: "refs/contexts/heads/main", SourceSHA: "source", Message: "context", Author: "tester", CreatedAt: time.Now().UTC(), Notes: []domain.Note{legacy}}
	identifier := writeTestContextObject(t, contextStore, object)

	chain, err := contextStore.loadObjectGraph(identifier, object.RepositoryID, object.RefName)
	if err != nil {
		t.Fatalf("loadObjectGraph() error = %v", err)
	}
	if len(chain) != 1 || len(chain[0].Object.Notes) != 1 {
		t.Fatalf("loadObjectGraph() = %#v", chain)
	}
	note := chain[0].Object.Notes[0]
	if note.RevisionID != legacy.ID || note.BindingState != domain.NoteBindingActive || note.BindingSourceSHA != object.SourceSHA {
		t.Fatalf("normalized legacy note = %+v", note)
	}
}

func TestLoadObjectGraphValidatesV3SnapshotManifest(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	sourceSHA := strings.Repeat("a", 40)
	note := domain.Note{ID: "note", RevisionID: "revision", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "snapshot evidence", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: sourceSHA}
	object := domain.ContextObject{
		SchemaVersion: 3,
		RepositoryID:  "repo",
		RefName:       "refs/contexts/heads/main",
		SourceSHA:     sourceSHA,
		Message:       "snapshot",
		Author:        "tester",
		CreatedAt:     time.Now().UTC(),
		Provenance: []domain.ContextSnapshotProvenance{{
			Language:       "go",
			IndexerID:      "builtin/go",
			IndexerVersion: "1",
			SourceSHA:      sourceSHA,
		}},
		Entities: []domain.Entity{{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: sourceSHA, StructuralHash: "hash"}},
		Notes:    []domain.Note{note},
		RevisionMappings: []domain.ContextRevisionMapping{{
			EntityKey:        note.EntityKey,
			NoteID:           note.ID,
			RevisionID:       note.RevisionID,
			BindingState:     note.BindingState,
			BindingSourceSHA: note.BindingSourceSHA,
			ReviewReason:     note.ReviewReason,
		}},
	}
	identifier := writeTestContextObject(t, contextStore, object)
	chain, err := contextStore.loadObjectGraph(identifier, object.RepositoryID, object.RefName)
	if err != nil || len(chain) != 1 || len(chain[0].Object.Provenance) != 1 || len(chain[0].Object.RevisionMappings) != 1 {
		t.Fatalf("loadObjectGraph() = %#v, %v", chain, err)
	}
	contents, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal v3 object: %v", err)
	}
	unknownField := []byte(strings.Replace(string(contents), `"repository_id":"repo"`, `"unexpected":true,"repository_id":"repo"`, 1))
	unknownID := writeRawContextObject(t, contextStore, unknownField)
	if _, err := contextStore.loadObjectGraph(unknownID, object.RepositoryID, object.RefName); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("loadObjectGraph() unknown v3 field error = %v, want validation", err)
	}
	object.Provenance = nil
	missingProvenanceID := writeTestContextObject(t, contextStore, object)
	if _, err := contextStore.loadObjectGraph(missingProvenanceID, object.RepositoryID, object.RefName); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("loadObjectGraph() missing provenance error = %v, want validation", err)
	}
	object.Provenance = []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: sourceSHA}}

	object.RevisionMappings[0].RevisionID = "wrong-revision"
	invalidID := writeTestContextObject(t, contextStore, object)
	if _, err := contextStore.loadObjectGraph(invalidID, object.RepositoryID, object.RefName); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("loadObjectGraph() invalid mapping error = %v, want validation", err)
	}
}

func TestLoadObjectGraphFollowsV3AdoptionLegacyParent(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	sourceSHA := strings.Repeat("a", 40)
	entity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: sourceSHA, StructuralHash: "hash"}
	legacyNote := domain.Note{ID: "note", RevisionID: "legacy-revision", EntityKey: entity.Key, Kind: domain.NoteIntent, Body: "legacy context", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: sourceSHA}
	legacy := domain.ContextObject{SchemaVersion: 2, RepositoryID: "repo", RefName: "refs/contexts/heads/main", SourceSHA: sourceSHA, Message: "legacy", Author: "tester", CreatedAt: time.Now().UTC(), Entities: []domain.Entity{entity}, Notes: []domain.Note{legacyNote}}
	legacyID := writeTestContextObject(t, contextStore, legacy)
	adoptedNote := legacyNote
	adoptedNote.RevisionID = "snapshot-revision"
	adoptedNote.SupersedesRevisionID = legacyNote.RevisionID
	adoptedNote.Body = "snapshot context"
	adopted := domain.ContextObject{
		SchemaVersion:  3,
		RepositoryID:   legacy.RepositoryID,
		RefName:        legacy.RefName,
		LegacyParentID: legacyID,
		SourceSHA:      sourceSHA,
		Message:        "adopt legacy",
		Author:         "tester",
		CreatedAt:      time.Now().UTC(),
		Provenance:     []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: sourceSHA}},
		Entities:       []domain.Entity{entity},
		Notes:          []domain.Note{adoptedNote},
		RevisionMappings: []domain.ContextRevisionMapping{{
			EntityKey:        adoptedNote.EntityKey,
			NoteID:           adoptedNote.ID,
			RevisionID:       adoptedNote.RevisionID,
			BindingState:     adoptedNote.BindingState,
			BindingSourceSHA: adoptedNote.BindingSourceSHA,
		}},
	}
	adoptedID := writeTestContextObject(t, contextStore, adopted)
	chain, err := contextStore.loadObjectGraph(adoptedID, legacy.RepositoryID, legacy.RefName)
	if err != nil || len(chain) != 2 || chain[0].ID != legacyID || chain[1].ID != adoptedID {
		t.Fatalf("loadObjectGraph() = %#v, %v", chain, err)
	}
}

func TestReadObjectGraphPreservesBothOrderedMergeParents(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	sourceSHA := strings.Repeat("a", 40)
	rootID := writeTestContextObject(t, contextStore, testV3SnapshotObject(sourceSHA, nil, nil))
	localNote := domain.Note{ID: "local", RevisionID: "local-revision", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "local context", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: sourceSHA}
	localID := writeTestContextObject(t, contextStore, testV3SnapshotObject(sourceSHA, []string{rootID}, []domain.Note{localNote}))
	remoteNote := domain.Note{ID: "remote", RevisionID: "remote-revision", EntityKey: "example.Run", Kind: domain.NoteWarning, Body: "remote context", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: sourceSHA}
	remoteID := writeTestContextObject(t, contextStore, testV3SnapshotObject(sourceSHA, []string{rootID}, []domain.Note{remoteNote}))
	mergeID := writeTestContextObject(t, contextStore, testV3SnapshotObject(sourceSHA, []string{localID, remoteID}, []domain.Note{localNote, remoteNote}))

	graph, err := contextStore.loadObjectGraph(mergeID, "repo", "refs/contexts/heads/main")
	if err != nil {
		t.Fatalf("loadObjectGraph() error = %v", err)
	}
	if len(graph) != 4 || graph[0].ID != rootID || graph[1].ID != localID || graph[2].ID != remoteID || graph[3].ID != mergeID {
		t.Fatalf("loadObjectGraph() = %+v", graph)
	}
	baseID, err := contextStore.FindMergeBase(localID, remoteID, "repo", "refs/contexts/heads/main")
	if err != nil || baseID != rootID {
		t.Fatalf("FindMergeBase() = %q, %v; want %q, nil", baseID, err, rootID)
	}
	if err := contextStore.withImmediate(ctx, func(conn *sql.Conn) error { return insertContextChain(ctx, conn, graph) }); err != nil {
		t.Fatalf("insertContextChain() error = %v", err)
	}
	rows, err := contextStore.db.QueryContext(ctx, "SELECT context_commit_id, parent_index, parent_id FROM context_commit_parents ORDER BY context_commit_id, parent_index")
	if err != nil {
		t.Fatalf("query ordered parents: %v", err)
	}
	defer rows.Close()
	var parents []string
	for rows.Next() {
		var commitID, parentID string
		var parentIndex int
		if err := rows.Scan(&commitID, &parentIndex, &parentID); err != nil {
			t.Fatalf("scan ordered parent: %v", err)
		}
		parents = append(parents, fmt.Sprintf("%s:%d:%s", commitID, parentIndex, parentID))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ordered parents: %v", err)
	}
	wantParents := []string{fmt.Sprintf("%s:0:%s", localID, rootID), fmt.Sprintf("%s:0:%s", mergeID, localID), fmt.Sprintf("%s:1:%s", mergeID, remoteID), fmt.Sprintf("%s:0:%s", remoteID, rootID)}
	sort.Strings(wantParents)
	if !reflect.DeepEqual(parents, wantParents) {
		t.Fatalf("ordered parents = %v, want %v", parents, wantParents)
	}
}

func TestCreateMergeSessionPersistsAutomaticRecordsAndConflicts(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	identifier := strings.Repeat("a", 64)
	record := domain.SnapshotMergeRecord{Note: domain.Note{ID: "automatic", RevisionID: "automatic-revision", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "automatic", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: strings.Repeat("b", 40)}, Mapping: domain.ContextRevisionMapping{EntityKey: "example.Run", NoteID: "automatic", RevisionID: "automatic-revision", BindingState: domain.NoteBindingActive, BindingSourceSHA: strings.Repeat("b", 40)}}
	session, err := contextStore.CreateMergeSession(ctx, domain.MergeSession{
		LocalSnapshotID:  identifier,
		RemoteSnapshotID: strings.Repeat("c", 64),
		BaseSnapshotID:   strings.Repeat("d", 64),
		RepositoryID:     "repo",
		RefName:          "refs/contexts/main",
		SourceSHA:        strings.Repeat("b", 40),
		Provenance:       []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: strings.Repeat("b", 40)}},
		Message:          "merge context",
		Author:           "tester",
		PlannedCreatedAt: time.Now().UTC(),
		AutomaticRecords: []domain.SnapshotMergeRecord{record},
		Conflicts:        []domain.MergeSessionConflict{{SnapshotMergeConflict: domain.SnapshotMergeConflict{NoteID: "conflict", Local: &record, Remote: &record}}},
	})
	if err != nil || session.ID == "" || session.State != domain.MergeSessionOpen || len(session.AutomaticRecords) != 1 || len(session.Conflicts) != 1 || session.Conflicts[0].ID != "conflict" || session.Conflicts[0].Resolution != domain.MergeConflictUnresolved {
		t.Fatalf("CreateMergeSession() = %+v, %v", session, err)
	}
	loaded, err := contextStore.MergeSession(ctx, session.ID)
	if err != nil || !reflect.DeepEqual(loaded, session) {
		t.Fatalf("MergeSession() = %+v, %v; want %+v, nil", loaded, err, session)
	}
}

func TestCreateMergeSessionRejectsDuplicateConflictIDs(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	record := domain.SnapshotMergeRecord{Note: domain.Note{ID: "one", RevisionID: "one-revision", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "one", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: strings.Repeat("b", 40)}, Mapping: domain.ContextRevisionMapping{EntityKey: "example.Run", NoteID: "one", RevisionID: "one-revision", BindingState: domain.NoteBindingActive, BindingSourceSHA: strings.Repeat("b", 40)}}
	_, err = contextStore.CreateMergeSession(ctx, domain.MergeSession{LocalSnapshotID: strings.Repeat("a", 64), RemoteSnapshotID: strings.Repeat("c", 64), BaseSnapshotID: strings.Repeat("d", 64), RepositoryID: "repo", RefName: "refs/contexts/main", SourceSHA: strings.Repeat("b", 40), Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: strings.Repeat("b", 40)}}, Message: "merge context", Author: "tester", PlannedCreatedAt: time.Now().UTC(), Conflicts: []domain.MergeSessionConflict{{ID: "duplicate", SnapshotMergeConflict: domain.SnapshotMergeConflict{NoteID: "one", Local: &record, Remote: &record}}, {ID: "duplicate", SnapshotMergeConflict: domain.SnapshotMergeConflict{NoteID: "two", Local: &record, Remote: &record}}}})
	if domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("CreateMergeSession() error = %v, want validation", err)
	}
}

func TestResolveMergeConflictTransitionsReadyOnlyAfterLastResolution(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	record := domain.SnapshotMergeRecord{Note: domain.Note{ID: "conflict", RevisionID: "local-revision", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "local", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: strings.Repeat("b", 40)}, Mapping: domain.ContextRevisionMapping{EntityKey: "example.Run", NoteID: "conflict", RevisionID: "local-revision", BindingState: domain.NoteBindingActive, BindingSourceSHA: strings.Repeat("b", 40)}}
	session, err := contextStore.CreateMergeSession(ctx, domain.MergeSession{LocalSnapshotID: strings.Repeat("a", 64), RemoteSnapshotID: strings.Repeat("c", 64), BaseSnapshotID: strings.Repeat("d", 64), RepositoryID: "repo", RefName: "refs/contexts/main", SourceSHA: strings.Repeat("b", 40), Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: strings.Repeat("b", 40)}}, Message: "merge context", Author: "tester", PlannedCreatedAt: time.Now().UTC(), Conflicts: []domain.MergeSessionConflict{{SnapshotMergeConflict: domain.SnapshotMergeConflict{NoteID: "conflict", Local: &record, Remote: &record}}}})
	if err != nil {
		t.Fatalf("CreateMergeSession() error = %v", err)
	}
	wrongSource := record
	wrongSource.Note.BindingSourceSHA = strings.Repeat("e", 40)
	wrongSource.Mapping.BindingSourceSHA = wrongSource.Note.BindingSourceSHA
	if _, err := contextStore.ResolveMergeConflict(ctx, session.ID, "conflict", domain.MergeConflictAuthored, &wrongSource); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ResolveMergeConflict(wrong source) error = %v, want validation", err)
	}
	stillOpen, err := contextStore.MergeSession(ctx, session.ID)
	if err != nil || stillOpen.State != domain.MergeSessionOpen || stillOpen.Conflicts[0].Resolution != domain.MergeConflictUnresolved {
		t.Fatalf("MergeSession() after rejected authored record = %+v, %v", stillOpen, err)
	}
	resolved, err := contextStore.ResolveMergeConflict(ctx, session.ID, "conflict", domain.MergeConflictLocal, nil)
	if err != nil || resolved.State != domain.MergeSessionReady || len(resolved.Conflicts) != 1 || resolved.Conflicts[0].Resolution != domain.MergeConflictLocal {
		t.Fatalf("ResolveMergeConflict() = %+v, %v", resolved, err)
	}
	if _, err := contextStore.ResolveMergeConflict(ctx, session.ID, "conflict", domain.MergeConflictRemote, nil); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ResolveMergeConflict() repeat error = %v, want validation", err)
	}
}

func TestFinalizeMergeAdvancesRefAndCommitsSessionAtomically(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: strings.Repeat("a", 40)}
	if err := contextStore.ApplyIndexUpdate(ctx, key, testProjection(key)); err != nil {
		t.Fatalf("ApplyIndexUpdate() error = %v", err)
	}
	localID := strings.Repeat("b", 64)
	remoteID := strings.Repeat("c", 64)
	mergedID := strings.Repeat("d", 64)
	if _, err := contextStore.db.ExecContext(ctx, `INSERT INTO context_commits (commit_id, parent_id, ref_name, source_sha, message, author, created_at) VALUES (?, '', ?, ?, 'local', 'tester', ?)`, localID, key.RefName, key.SourceSHA, time.Now().UTC().UnixNano()); err != nil {
		t.Fatalf("insert local commit: %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, "INSERT INTO context_refs (ref_name, commit_id, source_sha, version) VALUES (?, ?, ?, 1)", key.RefName, localID, key.SourceSHA); err != nil {
		t.Fatalf("insert local ref: %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, "UPDATE working_sets SET base_context_commit_id = ? WHERE worktree_id = ?", localID, key.WorktreeID); err != nil {
		t.Fatalf("set working parent: %v", err)
	}
	session, err := contextStore.CreateMergeSession(ctx, domain.MergeSession{LocalSnapshotID: localID, RemoteSnapshotID: remoteID, BaseSnapshotID: strings.Repeat("e", 64), RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: key.SourceSHA, Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA}}, Message: "merge", Author: "tester", PlannedCreatedAt: time.Now().UTC()})
	if err != nil || session.State != domain.MergeSessionReady {
		t.Fatalf("CreateMergeSession() = %+v, %v", session, err)
	}
	if err := contextStore.FinalizeMerge(ctx, FinalizeMergeInput{Key: key, SessionID: session.ID, Commit: domain.ContextCommit{ID: mergedID, ParentID: localID, RefName: key.RefName, SourceSHA: key.SourceSHA, Message: "merge", Author: "tester", CreatedAt: session.PlannedCreatedAt}, ParentIDs: []string{localID, remoteID}}); err != nil {
		t.Fatalf("FinalizeMerge() error = %v", err)
	}
	status, err := contextStore.Status(ctx, key)
	if err != nil || status.ContextCommitID != mergedID {
		t.Fatalf("Status() = %+v, %v", status, err)
	}
	updated, err := contextStore.MergeSession(ctx, session.ID)
	if err != nil || updated.State != domain.MergeSessionCommitted {
		t.Fatalf("MergeSession() = %+v, %v", updated, err)
	}
	var parents int
	if err := contextStore.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM context_commit_parents WHERE context_commit_id = ?", mergedID).Scan(&parents); err != nil || parents != 2 {
		t.Fatalf("merged parent rows = %d, %v; want 2, nil", parents, err)
	}
}

func TestFinalizeMergeCASLossLeavesWrittenObjectUnreachableAndSessionReady(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: strings.Repeat("a", 40)}
	if err := contextStore.ApplyIndexUpdate(ctx, key, testProjection(key)); err != nil {
		t.Fatalf("ApplyIndexUpdate() error = %v", err)
	}
	localID := strings.Repeat("b", 64)
	remoteID := strings.Repeat("c", 64)
	racedID := strings.Repeat("d", 64)
	if _, err := contextStore.db.ExecContext(ctx, `INSERT INTO context_commits (commit_id, parent_id, ref_name, source_sha, message, author, created_at) VALUES (?, '', ?, ?, 'local', 'tester', ?)`, localID, key.RefName, key.SourceSHA, time.Now().UTC().UnixNano()); err != nil {
		t.Fatalf("insert local commit: %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, `INSERT INTO context_commits (commit_id, parent_id, ref_name, source_sha, message, author, created_at) VALUES (?, '', ?, ?, 'raced', 'tester', ?)`, racedID, key.RefName, key.SourceSHA, time.Now().UTC().UnixNano()); err != nil {
		t.Fatalf("insert raced commit: %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, "INSERT INTO context_refs (ref_name, commit_id, source_sha, version) VALUES (?, ?, ?, 1)", key.RefName, localID, key.SourceSHA); err != nil {
		t.Fatalf("insert local ref: %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, "UPDATE working_sets SET base_context_commit_id = ? WHERE worktree_id = ?", localID, key.WorktreeID); err != nil {
		t.Fatalf("set working parent: %v", err)
	}
	session, err := contextStore.CreateMergeSession(ctx, domain.MergeSession{LocalSnapshotID: localID, RemoteSnapshotID: remoteID, BaseSnapshotID: strings.Repeat("e", 64), RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: key.SourceSHA, Provenance: []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA}}, Message: "merge", Author: "tester", PlannedCreatedAt: time.Now().UTC()})
	if err != nil || session.State != domain.MergeSessionReady {
		t.Fatalf("CreateMergeSession() = %+v, %v", session, err)
	}
	writtenID := writeRawContextObject(t, contextStore, []byte("unreachable merge object"))
	if _, err := contextStore.db.ExecContext(ctx, "UPDATE context_refs SET commit_id = ?, version = version + 1 WHERE ref_name = ?", racedID, key.RefName); err != nil {
		t.Fatalf("race local ref: %v", err)
	}
	err = contextStore.FinalizeMerge(ctx, FinalizeMergeInput{Key: key, SessionID: session.ID, Commit: domain.ContextCommit{ID: writtenID, ParentID: localID, RefName: key.RefName, SourceSHA: key.SourceSHA, Message: "merge", Author: "tester", CreatedAt: session.PlannedCreatedAt}, ParentIDs: []string{localID, remoteID}})
	if domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("FinalizeMerge() error = %v, want concurrent update", err)
	}
	if _, err := os.Stat(filepath.Join(contextStore.layout.ObjectDir, writtenID+".json")); err != nil {
		t.Fatalf("written object missing: %v", err)
	}
	var commitCount int
	if err := contextStore.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM context_commits WHERE commit_id = ?", writtenID).Scan(&commitCount); err != nil || commitCount != 0 {
		t.Fatalf("written object commit metadata = %d, %v; want 0, nil", commitCount, err)
	}
	status, err := contextStore.Status(ctx, key)
	if err != nil || status.ContextCommitID != racedID {
		t.Fatalf("Status() = %+v, %v", status, err)
	}
	updated, err := contextStore.MergeSession(ctx, session.ID)
	if err != nil || updated.State != domain.MergeSessionReady {
		t.Fatalf("MergeSession() = %+v, %v", updated, err)
	}
	if _, err := contextStore.db.ExecContext(ctx, "UPDATE context_refs SET commit_id = ?, version = version + 1 WHERE ref_name = ?", localID, key.RefName); err != nil {
		t.Fatalf("restore local ref for explicit retry: %v", err)
	}
	if err := contextStore.FinalizeMerge(ctx, FinalizeMergeInput{Key: key, SessionID: session.ID, Commit: domain.ContextCommit{ID: writtenID, ParentID: localID, RefName: key.RefName, SourceSHA: key.SourceSHA, Message: "merge", Author: "tester", CreatedAt: session.PlannedCreatedAt}, ParentIDs: []string{localID, remoteID}}); err != nil {
		t.Fatalf("FinalizeMerge(explicit retry) error = %v", err)
	}
	finalSession, err := contextStore.MergeSession(ctx, session.ID)
	if err != nil || finalSession.State != domain.MergeSessionCommitted {
		t.Fatalf("MergeSession() after retry = %+v, %v", finalSession, err)
	}
}

func testV3SnapshotObject(sourceSHA string, parentIDs []string, notes []domain.Note) domain.ContextObject {
	mappings := make([]domain.ContextRevisionMapping, 0, len(notes))
	for _, note := range notes {
		mappings = append(mappings, domain.ContextRevisionMapping{EntityKey: note.EntityKey, NoteID: note.ID, RevisionID: note.RevisionID, BindingState: note.BindingState, BindingSourceSHA: note.BindingSourceSHA, ReviewReason: note.ReviewReason})
	}
	return domain.ContextObject{
		SchemaVersion:    3,
		RepositoryID:     "repo",
		RefName:          "refs/contexts/heads/main",
		ParentIDs:        parentIDs,
		SourceSHA:        sourceSHA,
		Message:          "snapshot",
		Author:           "tester",
		CreatedAt:        time.Now().UTC(),
		Provenance:       []domain.ContextSnapshotProvenance{{Language: "go", IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: sourceSHA}},
		Entities:         []domain.Entity{{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: sourceSHA, StructuralHash: "hash"}},
		Notes:            notes,
		RevisionMappings: mappings,
	}
}

func testProjection(key domain.WorkingSetKey) []domain.LanguageProjection {
	return []domain.LanguageProjection{{
		Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA},
		Entities: []domain.Entity{{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: key.SourceSHA, StructuralHash: "hash"}},
	}}
}

func TestApplyIndexUpdateRebuildsSearchWithinImmediateTransaction(t *testing.T) {
	contextStore, err := Open(context.Background(), NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/heads/main", SourceSHA: "source"}

	if err := contextStore.ApplyIndexUpdate(ctx, key, testProjection(key)); err != nil {
		t.Fatalf("ApplyIndexUpdate() error = %v", err)
	}
}

func TestOpenDropsRemovedRelationProjectionTables(t *testing.T) {
	ctx := context.Background()
	layout := NewLayout(t.TempDir())
	contextStore, err := Open(ctx, layout)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	for _, statement := range []string{
		"CREATE TABLE entity_relations (worktree_id TEXT)",
		"CREATE TABLE relation_coverage (worktree_id TEXT)",
	} {
		if _, err := contextStore.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed removed table: %v", err)
		}
	}
	if err := contextStore.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	contextStore, err = Open(ctx, layout)
	if err != nil {
		t.Fatalf("Open(reopen) error = %v", err)
	}
	defer contextStore.Close()
	var count int
	if err := contextStore.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('entity_relations', 'relation_coverage')").Scan(&count); err != nil {
		t.Fatalf("inspect schema: %v", err)
	}
	if count != 0 {
		t.Fatalf("removed relation projection table count = %d, want 0", count)
	}
}

func writeTestContextObject(t *testing.T, contextStore *Store, object domain.ContextObject) string {
	t.Helper()
	contents, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal object: %v", err)
	}
	return writeRawContextObject(t, contextStore, contents)
}

func writeRawContextObject(t *testing.T, contextStore *Store, contents []byte) string {
	t.Helper()
	digest := blake3.Sum256(contents)
	identifier := fmt.Sprintf("%x", digest[:])
	if err := contextStore.WriteObject(identifier, contents); err != nil {
		t.Fatalf("WriteObject() error = %v", err)
	}
	return identifier
}

func assertEmptyProjection(t *testing.T, contextStore *Store, key domain.WorkingSetKey) {
	t.Helper()
	status, err := contextStore.Status(context.Background(), key)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.ContextCommitID != "" || status.EntityCount != 0 || status.PendingNotes != 0 || len(status.Coverage) != 0 {
		t.Fatalf("projection is not empty: %+v", status)
	}
	log, err := contextStore.Log(context.Background(), key, 10)
	if err != nil {
		t.Fatalf("Log() error = %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("Log() = %+v, want empty", log)
	}
}

func TestApplyIndexUpdateKeepsFailedProjectionOutOfCurrentSearch(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: "source"}
	goEntity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: key.SourceSHA, StructuralHash: "go-hash"}
	typeScriptEntity := domain.Entity{Language: "typescript", Key: "typescript:web/app.ts#function:run", Kind: domain.EntityFunction, Name: "run", Path: "web/app.ts", SourceSHA: key.SourceSHA, StructuralHash: "ts-hash"}

	err = contextStore.ApplyIndexUpdate(ctx, key, []domain.LanguageProjection{
		{Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA}, Entities: []domain.Entity{goEntity}},
		{Coverage: domain.Coverage{Language: "typescript", State: domain.CoverageIndexed, IndexerID: "thread-keep-index-typescript", IndexerVersion: "1", SourceSHA: key.SourceSHA}, Entities: []domain.Entity{typeScriptEntity}},
	})
	if err != nil {
		t.Fatalf("seed ApplyIndexUpdate() error = %v", err)
	}
	err = contextStore.ApplyIndexUpdate(ctx, key, []domain.LanguageProjection{
		{Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA}, Entities: []domain.Entity{goEntity}},
		{Coverage: domain.Coverage{Language: "typescript", State: domain.CoverageFailed, IndexerID: "thread-keep-index-typescript", IndexerVersion: "1", SourceSHA: key.SourceSHA, Detail: "syntax error"}},
	})
	if err != nil {
		t.Fatalf("ApplyIndexUpdate() error = %v", err)
	}

	status, err := contextStore.Status(ctx, key)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.EntityCount != 1 || status.CoverageComplete {
		t.Fatalf("status = %+v, want one fresh entity and incomplete coverage", status)
	}
	wantCoverage := []domain.Coverage{
		{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA},
		{Language: "typescript", State: domain.CoverageFailed, IndexerID: "thread-keep-index-typescript", IndexerVersion: "1", SourceSHA: key.SourceSHA, Detail: "syntax error"},
	}
	if !reflect.DeepEqual(status.Coverage, wantCoverage) {
		t.Fatalf("Coverage = %#v, want %#v", status.Coverage, wantCoverage)
	}
	hits, err := contextStore.Search(ctx, key, "run")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 1 || hits[0].EntityKey != goEntity.Key || !hits[0].Fresh {
		t.Fatalf("Search() = %#v, want only Go entity", hits)
	}
}

func TestRemoteConfigurationPersistsOutsideProjectionState(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	if err := contextStore.Initialize(ctx); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	remote, err := contextStore.AddRemote(ctx, domain.Remote{Name: "origin", Path: t.TempDir()})
	if err != nil {
		t.Fatalf("AddRemote() error = %v", err)
	}
	if remote.Name != "origin" || remote.Path == "" {
		t.Fatalf("AddRemote() = %+v", remote)
	}
	remotes, err := contextStore.Remotes(ctx)
	if err != nil || !reflect.DeepEqual(remotes, []domain.Remote{remote}) {
		t.Fatalf("Remotes() = %#v, %v; want %#v, nil", remotes, err, []domain.Remote{remote})
	}
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: "source"}
	if err := contextStore.RecordRemoteRef(ctx, domain.RemoteRef{RemoteName: remote.Name, RefName: key.RefName, CommitID: strings.Repeat("a", 64), SourceSHA: key.SourceSHA, Version: 1}); err != nil {
		t.Fatalf("RecordRemoteRef() error = %v", err)
	}
	var tracking domain.RemoteRef
	tracking.RemoteName = remote.Name
	tracking.RefName = key.RefName
	err = contextStore.db.QueryRowContext(ctx, "SELECT commit_id, source_sha, version FROM remote_refs WHERE remote_name = ? AND ref_name = ?", remote.Name, key.RefName).Scan(&tracking.CommitID, &tracking.SourceSHA, &tracking.Version)
	if err != nil || tracking.CommitID != strings.Repeat("a", 64) || tracking.SourceSHA != key.SourceSHA || tracking.Version != 1 {
		t.Fatalf("remote tracking row = %+v, %v", tracking, err)
	}
	conn, err := contextStore.db.Conn(ctx)
	if err != nil {
		t.Fatalf("open database connection: %v", err)
	}
	defer conn.Close()
	if empty, err := projectionEmpty(ctx, conn); err != nil || !empty {
		t.Fatalf("projectionEmpty() = %t, %v; want true, nil", empty, err)
	}
}

func TestCandidateImportKeepsDraftNotesOutOfCanonicalProjection(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	if err := contextStore.Initialize(ctx); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	candidate := domain.Candidate{ID: "github:owner/repository#42", Provider: "github", Repository: "owner/repository", Number: 42, State: domain.CandidateOpen, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), UpdatedAt: time.Now().UTC()}
	note := domain.CandidateNote{CandidateID: candidate.ID, ID: "review-note", EntityKey: "example.Run", StructuralHash: strings.Repeat("c", 64), Kind: domain.NoteIntent, Body: "candidate draft", Author: "reviewer", Origin: "provider", CreatedAt: candidate.UpdatedAt, State: domain.CandidateNoteDraft}
	imported, err := contextStore.ImportCandidate(ctx, candidate, []domain.CandidateNote{note})
	if err != nil || !imported {
		t.Fatalf("ImportCandidate() = %t, %v; want true, nil", imported, err)
	}
	stored, notes, err := contextStore.Candidate(ctx, candidate.ID)
	if err != nil || !reflect.DeepEqual(stored, candidate) || !reflect.DeepEqual(notes, []domain.CandidateNote{note}) {
		t.Fatalf("Candidate() = %#v, %#v, %v", stored, notes, err)
	}
	if empty, err := projectionEmpty(ctx, mustConn(t, contextStore, ctx)); err != nil || !empty {
		t.Fatalf("projectionEmpty() = %t, %v; want true, nil", empty, err)
	}
}

func TestCandidateImportIsMonotonicAndReplacesOnlyDraftNotes(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	if err := contextStore.Initialize(ctx); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	updatedAt := time.Now().UTC().Truncate(time.Nanosecond)
	candidate := domain.Candidate{ID: "github:owner/repository#42", Provider: "github", Repository: "owner/repository", Number: 42, State: domain.CandidateOpen, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), UpdatedAt: updatedAt}
	first := domain.CandidateNote{CandidateID: candidate.ID, ID: "first", EntityKey: "example.Run", StructuralHash: strings.Repeat("c", 64), Kind: domain.NoteIntent, Body: "first draft", Author: "reviewer", Origin: "provider", CreatedAt: updatedAt, State: domain.CandidateNoteDraft}
	if imported, err := contextStore.ImportCandidate(ctx, candidate, []domain.CandidateNote{first}); err != nil || !imported {
		t.Fatalf("initial ImportCandidate() = %t, %v", imported, err)
	}
	if imported, err := contextStore.ImportCandidate(ctx, candidate, []domain.CandidateNote{first}); err != nil || imported {
		t.Fatalf("same ImportCandidate() = %t, %v; want false, nil", imported, err)
	}
	older := candidate
	older.UpdatedAt = older.UpdatedAt.Add(-time.Nanosecond)
	if _, err := contextStore.ImportCandidate(ctx, older, []domain.CandidateNote{first}); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("older ImportCandidate() error = %v, want validation", err)
	}
	newer := candidate
	newer.UpdatedAt = newer.UpdatedAt.Add(time.Second)
	second := first
	second.ID = "second"
	second.Body = "newer draft"
	second.CreatedAt = newer.UpdatedAt
	if imported, err := contextStore.ImportCandidate(ctx, newer, []domain.CandidateNote{second}); err != nil || !imported {
		t.Fatalf("newer ImportCandidate() = %t, %v", imported, err)
	}
	stored, notes, err := contextStore.Candidate(ctx, candidate.ID)
	if err != nil || !reflect.DeepEqual(stored, newer) || !reflect.DeepEqual(notes, []domain.CandidateNote{second}) {
		t.Fatalf("Candidate() = %#v, %#v, %v", stored, notes, err)
	}
}

func TestNewerCandidateImportPreservesPromotedNoteIdentity(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	if err := contextStore.Initialize(ctx); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	updatedAt := time.Now().UTC()
	candidate := domain.Candidate{ID: "github:owner/repository#42", Provider: "github", Repository: "owner/repository", Number: 42, State: domain.CandidateMerged, BaseSHA: "base", HeadSHA: "head", MergeSHA: "merge", UpdatedAt: updatedAt}
	note := domain.CandidateNote{CandidateID: candidate.ID, ID: "note", EntityKey: "example.Run", StructuralHash: strings.Repeat("c", 64), Kind: domain.NoteIntent, Body: "original", Author: "reviewer", Origin: "provider", CreatedAt: updatedAt, State: domain.CandidateNoteDraft}
	if _, err := contextStore.ImportCandidate(ctx, candidate, []domain.CandidateNote{note}); err != nil {
		t.Fatalf("ImportCandidate() error = %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, "UPDATE candidate_notes SET state = ?, promoted_note_id = ? WHERE candidate_id = ? AND note_id = ?", domain.CandidateNotePromoted, "canonical-note", candidate.ID, note.ID); err != nil {
		t.Fatalf("mark promoted note: %v", err)
	}
	newer := candidate
	newer.UpdatedAt = newer.UpdatedAt.Add(time.Second)
	changed := note
	changed.Body = "provider update must not overwrite promoted identity"
	changed.CreatedAt = newer.UpdatedAt
	if imported, err := contextStore.ImportCandidate(ctx, newer, []domain.CandidateNote{changed}); err != nil || !imported {
		t.Fatalf("newer ImportCandidate() = %t, %v", imported, err)
	}
	_, notes, err := contextStore.Candidate(ctx, candidate.ID)
	if err != nil || len(notes) != 1 || notes[0].State != domain.CandidateNotePromoted || notes[0].PromotedNoteID != "canonical-note" || notes[0].Body != note.Body {
		t.Fatalf("Candidate() after newer import = %+v, %v", notes, err)
	}
}

func TestCandidatePromotionRollsBackPendingNotesWhenOutcomeWriteFails(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	if err := contextStore.Initialize(ctx); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: "source"}
	entity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: key.SourceSHA, StructuralHash: "hash"}
	if err := contextStore.ApplyIndexUpdate(ctx, key, []domain.LanguageProjection{{Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA}, Entities: []domain.Entity{entity}}}); err != nil {
		t.Fatalf("ApplyIndexUpdate() error = %v", err)
	}
	candidate := domain.Candidate{ID: "github:owner/repository#42", Provider: "github", Repository: "owner/repository", Number: 42, State: domain.CandidateMerged, BaseSHA: "base", HeadSHA: "head", MergeSHA: key.SourceSHA, UpdatedAt: time.Now().UTC()}
	note := domain.CandidateNote{CandidateID: candidate.ID, ID: "note", EntityKey: entity.Key, StructuralHash: entity.StructuralHash, Kind: domain.NoteIntent, Body: "promote atomically", Author: "reviewer", Origin: "provider", CreatedAt: candidate.UpdatedAt, State: domain.CandidateNoteDraft}
	if _, err := contextStore.ImportCandidate(ctx, candidate, []domain.CandidateNote{note}); err != nil {
		t.Fatalf("ImportCandidate() error = %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, `CREATE TRIGGER fail_candidate_outcome BEFORE UPDATE ON candidate_notes BEGIN SELECT RAISE(ABORT, 'forced candidate outcome failure'); END`); err != nil {
		t.Fatalf("create candidate trigger: %v", err)
	}
	if _, err := contextStore.PromoteCandidate(ctx, key, candidate.ID); domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("PromoteCandidate() error = %v, want local storage", err)
	}
	pending, err := contextStore.PendingNotes(ctx, key)
	if err != nil || len(pending) != 0 {
		t.Fatalf("PendingNotes() after failed promotion = %+v, %v; want empty, nil", pending, err)
	}
	_, notes, err := contextStore.Candidate(ctx, candidate.ID)
	if err != nil || len(notes) != 1 || notes[0].State != domain.CandidateNoteDraft || notes[0].PromotedNoteID != "" {
		t.Fatalf("Candidate() after failed promotion = %+v, %v", notes, err)
	}
}

func TestCandidatePromotionRejectsUnmergedCandidatesWithoutPendingNotes(t *testing.T) {
	for _, state := range []domain.CandidateState{domain.CandidateOpen, domain.CandidateClosed} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			contextStore, err := Open(ctx, NewLayout(t.TempDir()))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer contextStore.Close()
			if err := contextStore.Initialize(ctx); err != nil {
				t.Fatalf("Initialize() error = %v", err)
			}
			key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: "source"}
			entity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: key.SourceSHA, StructuralHash: "hash"}
			if err := contextStore.ApplyIndexUpdate(ctx, key, []domain.LanguageProjection{{Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA}, Entities: []domain.Entity{entity}}}); err != nil {
				t.Fatalf("ApplyIndexUpdate() error = %v", err)
			}
			candidate := domain.Candidate{ID: "github:owner/repository#42", Provider: "github", Repository: "owner/repository", Number: 42, State: state, BaseSHA: "base", HeadSHA: "head", UpdatedAt: time.Now().UTC()}
			note := domain.CandidateNote{CandidateID: candidate.ID, ID: "note", EntityKey: entity.Key, StructuralHash: entity.StructuralHash, Kind: domain.NoteIntent, Body: "not merged", Author: "reviewer", Origin: "provider", CreatedAt: candidate.UpdatedAt, State: domain.CandidateNoteDraft}
			if _, err := contextStore.ImportCandidate(ctx, candidate, []domain.CandidateNote{note}); err != nil {
				t.Fatalf("ImportCandidate() error = %v", err)
			}
			if _, err := contextStore.PromoteCandidate(ctx, key, candidate.ID); domain.CodeOf(err) != domain.CodeValidation {
				t.Fatalf("PromoteCandidate(%s) error = %v, want validation", state, err)
			}
			pending, err := contextStore.PendingNotes(ctx, key)
			if err != nil || len(pending) != 0 {
				t.Fatalf("PendingNotes() after rejected promotion = %+v, %v; want empty, nil", pending, err)
			}
		})
	}
}

func mustConn(t *testing.T, contextStore *Store, ctx context.Context) *sql.Conn {
	t.Helper()
	conn, err := contextStore.db.Conn(ctx)
	if err != nil {
		t.Fatalf("open database connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestApplyIndexUpdateRollsBackWhenBindingReconciliationWriteFails(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	oldKey := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: "old-source"}
	oldProjection := testProjection(oldKey)
	if err := contextStore.ApplyIndexUpdate(ctx, oldKey, oldProjection); err != nil {
		t.Fatalf("ApplyIndexUpdate(old) error = %v", err)
	}
	legacy := domain.Note{ID: "note", RevisionID: "revision", EntityKey: "example.Run", Kind: domain.NoteIntent, Body: "keep behavior", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: oldKey.SourceSHA}
	object := domain.ContextObject{SchemaVersion: 2, RepositoryID: oldKey.RepositoryID, RefName: oldKey.RefName, SourceSHA: oldKey.SourceSHA, Message: "context", Author: "tester", CreatedAt: time.Now().UTC(), Entities: oldProjection[0].Entities, Notes: []domain.Note{legacy}}
	identifier := writeTestContextObject(t, contextStore, object)
	if _, err := contextStore.db.ExecContext(ctx, `INSERT INTO context_commits (commit_id, parent_id, ref_name, source_sha, message, author, created_at) VALUES (?, '', ?, ?, ?, ?, ?)`, identifier, oldKey.RefName, oldKey.SourceSHA, object.Message, object.Author, object.CreatedAt.UnixNano()); err != nil {
		t.Fatalf("insert context commit: %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, `INSERT INTO context_refs (ref_name, commit_id, source_sha, version) VALUES (?, ?, ?, 1)`, oldKey.RefName, identifier, oldKey.SourceSHA); err != nil {
		t.Fatalf("insert context ref: %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, `INSERT INTO committed_notes (context_commit_id, note_id, revision_id, supersedes_revision_id, entity_key, kind, body, author, origin, created_at, binding_state, binding_source_sha, review_reason) VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, '')`, identifier, legacy.ID, legacy.RevisionID, legacy.EntityKey, legacy.Kind, legacy.Body, legacy.Author, legacy.Origin, legacy.CreatedAt.UnixNano(), legacy.BindingState, legacy.BindingSourceSHA); err != nil {
		t.Fatalf("insert committed note: %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, `UPDATE working_sets SET base_context_commit_id = ? WHERE worktree_id = ?`, identifier, oldKey.WorktreeID); err != nil {
		t.Fatalf("set working parent: %v", err)
	}
	if _, err := contextStore.db.ExecContext(ctx, `CREATE TRIGGER fail_binding_reconciliation BEFORE INSERT ON pending_notes BEGIN SELECT RAISE(ABORT, 'forced reconciliation failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	newKey := oldKey
	newKey.SourceSHA = "new-source"
	newProjection := testProjection(newKey)
	newProjection[0].Entities[0].StructuralHash = "changed-hash"
	if err := contextStore.ApplyIndexUpdate(ctx, newKey, newProjection); domain.CodeOf(err) != domain.CodeLocalStorage {
		t.Fatalf("ApplyIndexUpdate(new) error = %v, want local storage", err)
	}
	var workingSource string
	if err := contextStore.db.QueryRowContext(ctx, "SELECT source_sha FROM working_sets WHERE worktree_id = ?", oldKey.WorktreeID).Scan(&workingSource); err != nil {
		t.Fatalf("read working source: %v", err)
	}
	if workingSource != oldKey.SourceSHA {
		t.Fatalf("working source after rollback = %q, want %q", workingSource, oldKey.SourceSHA)
	}
	var structuralHash string
	if err := contextStore.db.QueryRowContext(ctx, "SELECT structural_hash FROM entities WHERE worktree_id = ? AND entity_key = ?", oldKey.WorktreeID, "example.Run").Scan(&structuralHash); err != nil {
		t.Fatalf("read entity after rollback: %v", err)
	}
	if structuralHash != oldProjection[0].Entities[0].StructuralHash {
		t.Fatalf("entity hash after rollback = %q, want %q", structuralHash, oldProjection[0].Entities[0].StructuralHash)
	}
}

func TestRebuildRestoresMixedSchemaNoteAncestry(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: "current-source"}
	entity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: "parent-source", StructuralHash: "hash"}
	legacy := domain.Note{ID: "legacy", EntityKey: entity.Key, Kind: domain.NoteIntent, Body: "legacy body", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC()}
	parent := domain.ContextObject{SchemaVersion: 1, RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: "parent-source", Message: "legacy", Author: "tester", CreatedAt: time.Now().UTC(), Entities: []domain.Entity{entity}, Notes: []domain.Note{legacy}}
	parentID := writeTestContextObject(t, contextStore, parent)
	normalizedLegacy := domain.NormalizeLegacyNote(legacy, parent.SourceSHA)
	newNote := domain.Note{ID: "new", RevisionID: "new-revision", EntityKey: entity.Key, Kind: domain.NoteWarning, Body: "new body", Author: "reviewer", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: parent.SourceSHA}
	child := domain.ContextObject{SchemaVersion: 2, RepositoryID: key.RepositoryID, RefName: key.RefName, ParentID: parentID, SourceSHA: parent.SourceSHA, Message: "v2", Author: "reviewer", CreatedAt: time.Now().UTC(), Entities: []domain.Entity{entity}, Notes: []domain.Note{normalizedLegacy, newNote}}
	childID := writeTestContextObject(t, contextStore, child)
	if _, restored, err := contextStore.Rebuild(ctx, RebuildInput{Key: key, CommitID: childID, Projections: testProjection(key)}); err != nil || restored != 2 {
		t.Fatalf("Rebuild() restored=%d error=%v, want 2 and nil", restored, err)
	}
	_, notes, err := contextStore.Context(ctx, key, entity.Key)
	if err != nil {
		t.Fatalf("Context() error = %v", err)
	}
	if len(notes) != 2 || notes[0].RevisionID == "" || notes[1].RevisionID != newNote.RevisionID {
		t.Fatalf("restored mixed-schema notes = %+v", notes)
	}
}

func TestReadContextObjectRejectsChangedContentForRepeatedRevisionID(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer contextStore.Close()
	key := domain.WorkingSetKey{RepositoryID: "repo", RefName: "refs/contexts/main"}
	sourceSHA := "source"
	entity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", StartLine: 1, EndLine: 1, SourceSHA: sourceSHA, StructuralHash: "hash"}
	note := domain.Note{ID: "note", RevisionID: "revision", EntityKey: entity.Key, Kind: domain.NoteDecision, Body: "original decision", Author: "tester", Origin: "human", CreatedAt: time.Now().UTC(), BindingState: domain.NoteBindingActive, BindingSourceSHA: sourceSHA}
	parent := domain.ContextObject{SchemaVersion: 2, RepositoryID: key.RepositoryID, RefName: key.RefName, SourceSHA: sourceSHA, Message: "parent", Author: "tester", CreatedAt: time.Now().UTC(), Entities: []domain.Entity{entity}, Notes: []domain.Note{note}}
	parentID := writeTestContextObject(t, contextStore, parent)
	changed := note
	changed.Body = "silently changed decision"
	child := domain.ContextObject{SchemaVersion: 2, RepositoryID: key.RepositoryID, RefName: key.RefName, ParentID: parentID, SourceSHA: sourceSHA, Message: "child", Author: "tester", CreatedAt: time.Now().UTC(), Entities: []domain.Entity{entity}, Notes: []domain.Note{changed}}
	childID := writeTestContextObject(t, contextStore, child)

	if _, err := contextStore.ReadContextObject(childID, key.RepositoryID, key.RefName); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ReadContextObject() error = %v, want repeated revision mutation rejection", err)
	}
}

func TestFinalizeCommitRejectsChangedPendingNotes(t *testing.T) {
	ctx := context.Background()
	contextStore, err := Open(ctx, NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })
	if err := contextStore.Initialize(ctx); err != nil {
		t.Fatalf("initialize store: %v", err)
	}

	key := domain.WorkingSetKey{RepositoryID: "repo", WorktreeID: "worktree", RefName: "refs/contexts/main", SourceSHA: "source"}
	entity := domain.Entity{Language: "go", Key: "example.Run", Kind: domain.EntityFunction, Name: "Run", Path: "example.go", SourceSHA: key.SourceSHA, StructuralHash: "hash"}
	if err := contextStore.ApplyIndexUpdate(ctx, key, []domain.LanguageProjection{{
		Coverage: domain.Coverage{Language: "go", State: domain.CoverageIndexed, IndexerID: "builtin/go", IndexerVersion: "1", SourceSHA: key.SourceSHA},
		Entities: []domain.Entity{entity},
	}}); err != nil {
		t.Fatalf("ApplyIndexUpdate() error = %v", err)
	}
	first, err := contextStore.AddPendingNote(ctx, key, domain.Note{EntityKey: entity.Key, Kind: domain.NoteIntent, Body: "first"})
	if err != nil {
		t.Fatalf("add first note: %v", err)
	}
	snapshot, err := contextStore.CommitSnapshot(ctx, key)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	second, err := contextStore.AddPendingNote(ctx, key, domain.Note{EntityKey: entity.Key, Kind: domain.NoteDecision, Body: "second"})
	if err != nil {
		t.Fatalf("add second note: %v", err)
	}

	err = contextStore.FinalizeCommit(ctx, FinalizeInput{
		Key:            key,
		ExpectedParent: snapshot.ParentID,
		PendingNoteIDs: []string{first.ID},
		Commit: domain.ContextCommit{
			ID:        "commit-1",
			RefName:   key.RefName,
			SourceSHA: key.SourceSHA,
			Message:   "test",
			Author:    "tester",
			CreatedAt: time.Now().UTC(),
		},
		Notes: []domain.Note{first},
	})
	if domain.CodeOf(err) != domain.CodeConcurrentUpdate {
		t.Fatalf("finalize error code = %q, error=%v", domain.CodeOf(err), err)
	}
	pending, err := contextStore.PendingNotes(ctx, key)
	if err != nil {
		t.Fatalf("pending notes: %v", err)
	}
	if len(pending) != 2 || pending[0].ID != first.ID || pending[1].ID != second.ID {
		t.Fatalf("pending notes after rejected finalize = %#v", pending)
	}
}
