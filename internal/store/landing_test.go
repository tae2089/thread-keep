package store

import (
	"context"
	"strings"
	"testing"

	"github.com/tae2089/thread-keep/internal/domain"
)

type invalidV4ReceiptTest struct {
	name   string
	mutate func(*domain.ContextObject)
}

func TestReadContextObjectSupportsV4LandingReceipt(t *testing.T) {
	contextStore, err := Open(context.Background(), NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })
	root := testV3SnapshotObject("source", nil, nil)
	rootID := writeTestContextObject(t, contextStore, root)
	object := testV4LandingObject(root, rootID, testLandingReceipt(root, rootID))
	identifier := writeTestContextObject(t, contextStore, object)

	got, err := contextStore.ReadContextObject(identifier, object.RepositoryID, object.RefName)
	if err != nil {
		t.Fatalf("ReadContextObject() error = %v", err)
	}
	if got.SchemaVersion != 4 || len(got.LandingReceipts) != 1 || got.LandingReceipts[0].ID != object.LandingReceipts[0].ID {
		t.Fatalf("ReadContextObject() = %+v, want v4 receipt", got)
	}
}

func TestReadContextObjectRejectsInvalidV4LandingReceipt(t *testing.T) {
	contextStore, err := Open(context.Background(), NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })
	root := testV3SnapshotObject("source", nil, nil)
	rootID := writeTestContextObject(t, contextStore, root)
	tests := []invalidV4ReceiptTest{
		{name: "context repository mismatch", mutate: func(object *domain.ContextObject) { object.LandingReceipts[0].ContextRepositoryID = "other" }},
		{name: "target ref mismatch", mutate: func(object *domain.ContextObject) { object.LandingReceipts[0].TargetRef = "refs/contexts/other" }},
		{name: "source mismatch", mutate: func(object *domain.ContextObject) { object.LandingReceipts[0].SourceMergeSHA = "other-source" }},
		{name: "duplicate receipt", mutate: func(object *domain.ContextObject) {
			object.LandingReceipts = append(object.LandingReceipts, object.LandingReceipts[0])
		}},
		{name: "invalid candidate mapping", mutate: func(object *domain.ContextObject) { object.LandingReceipts[0].CandidateMappings[0].RevisionID = "" }},
		{name: "receipt on v3", mutate: func(object *domain.ContextObject) { object.SchemaVersion = 3 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := testV4LandingObject(root, rootID, testLandingReceipt(root, rootID))
			test.mutate(&object)
			identifier := writeTestContextObject(t, contextStore, object)
			if _, err := contextStore.ReadContextObject(identifier, object.RepositoryID, object.RefName); domain.CodeOf(err) != domain.CodeValidation {
				t.Fatalf("ReadContextObject() error = %v, want validation", err)
			}
		})
	}
}

func TestReadContextObjectRejectsDuplicateLandingReceiptInAncestry(t *testing.T) {
	contextStore, err := Open(context.Background(), NewLayout(t.TempDir()))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = contextStore.Close() })
	root := testV3SnapshotObject("source", nil, nil)
	rootID := writeTestContextObject(t, contextStore, root)
	receipt := testLandingReceipt(root, rootID)
	first := testV4LandingObject(root, rootID, receipt)
	firstID := writeTestContextObject(t, contextStore, first)
	second := testV4LandingObject(root, firstID, receipt)
	secondID := writeTestContextObject(t, contextStore, second)

	if _, err := contextStore.ReadContextObject(secondID, second.RepositoryID, second.RefName); domain.CodeOf(err) != domain.CodeValidation {
		t.Fatalf("ReadContextObject() error = %v, want duplicate receipt validation", err)
	}
}

func testV4LandingObject(base domain.ContextObject, parentID string, receipt domain.LandingReceipt) domain.ContextObject {
	object := base
	object.SchemaVersion = 4
	object.ParentIDs = []string{parentID}
	object.LandingReceipts = []domain.LandingReceipt{receipt}
	return object
}

func testLandingReceipt(object domain.ContextObject, baseContextCommitID string) domain.LandingReceipt {
	return domain.LandingReceipt{
		ID:                  strings.Repeat("a", 64),
		Provider:            "github",
		ForgeRepository:     "owner/repository",
		ChangeNumber:        42,
		ContextRepositoryID: object.RepositoryID,
		TargetRef:           object.RefName,
		CandidateDigest:     strings.Repeat("b", 64),
		FinalPlanID:         strings.Repeat("c", 64),
		SourceMergeSHA:      object.SourceSHA,
		BaseContextCommitID: baseContextCommitID,
		Resolver:            "automatic",
		CandidateMappings: []domain.CandidatePromotionMapping{{
			CandidateRecordID: "record-1",
			NoteID:            "note-1",
			RevisionID:        "revision-1",
		}},
	}
}
