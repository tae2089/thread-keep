package domain

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeRemoteName(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
		valid bool
	}{
		{input: " origin ", want: "origin", valid: true},
		{input: "team-remote_1", want: "team-remote_1", valid: true},
		{input: "", valid: false},
		{input: "../remote", valid: false},
		{input: "remote/name", valid: false},
		{input: "remote name", valid: false},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := NormalizeRemoteName(test.input)
			if test.valid {
				if err != nil || got != test.want {
					t.Fatalf("NormalizeRemoteName(%q) = %q, %v; want %q, nil", test.input, got, err, test.want)
				}
				return
			}
			if CodeOf(err) != CodeValidation {
				t.Fatalf("NormalizeRemoteName(%q) error = %v, want validation", test.input, err)
			}
		})
	}
}

func TestNormalizeNoteTopics(t *testing.T) {
	topics, err := NormalizeNoteTopics([]string{" Cache-Invalidation ", "캐시-무효화"})
	if err != nil {
		t.Fatalf("NormalizeNoteTopics() error = %v", err)
	}
	if !reflect.DeepEqual(topics, []string{"cache-invalidation", "캐시-무효화"}) {
		t.Fatalf("NormalizeNoteTopics() = %#v", topics)
	}
	if _, err := NormalizeNoteTopics([]string{"cache", " CACHE "}); CodeOf(err) != CodeValidation {
		t.Fatalf("NormalizeNoteTopics(duplicate) error = %v, want validation", err)
	}
}

func TestDecodeCandidateEnvelope(t *testing.T) {
	sourceSHA := strings.Repeat("a", 40)
	structuralHash := strings.Repeat("b", 64)
	valid := fmt.Sprintf(`{
  "schema_version": 1,
  "provider": "github",
  "repository": "owner/repository",
  "number": 42,
  "state": "merged",
  "base_sha": %q,
  "head_sha": %q,
  "merge_sha": %q,
  "updated_at": "2026-07-12T00:00:00Z",
  "notes": [{
    "id": "review-note-1",
    "entity_key": "example.Run",
    "structural_hash": %q,
    "kind": "intent",
    "body": "keep retry idempotent",
    "author": "reviewer",
    "origin": "provider",
    "created_at": "2026-07-12T00:00:00Z"
  }]
}`, sourceSHA, sourceSHA, sourceSHA, structuralHash)
	candidate, notes, err := DecodeCandidateEnvelope([]byte(valid))
	if err != nil {
		t.Fatalf("DecodeCandidateEnvelope() error = %v", err)
	}
	if candidate.ID != "github:owner/repository#42" || candidate.State != CandidateMerged || candidate.MergeSHA != sourceSHA || len(notes) != 1 {
		t.Fatalf("DecodeCandidateEnvelope() = %+v, %+v", candidate, notes)
	}
	if notes[0].CandidateID != candidate.ID || notes[0].State != CandidateNoteDraft || notes[0].StructuralHash != structuralHash {
		t.Fatalf("candidate note = %+v", notes[0])
	}

	for _, test := range []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{name: "duplicate note ID", mutate: func(value string) string {
			return strings.Replace(value, "  }]\n}", `  },{"id":"review-note-1","entity_key":"example.Run","structural_hash":"`+structuralHash+`","kind":"intent","body":"duplicate","author":"reviewer","origin":"provider","created_at":"2026-07-12T00:00:00Z"}]`+"\n}", 1)
		}, wantErr: "duplicate"},
		{name: "open with merge SHA", mutate: func(value string) string { return strings.Replace(value, `"state": "merged"`, `"state": "open"`, 1) }, wantErr: "open candidate"},
		{name: "internal note state", mutate: func(value string) string {
			return strings.Replace(value, `"created_at": "2026-07-12T00:00:00Z"`, `"created_at": "2026-07-12T00:00:00Z", "state": "promoted"`, 1)
		}, wantErr: "unknown field"},
		{name: "note ID with surrounding whitespace", mutate: func(value string) string {
			return strings.Replace(value, `"id": "review-note-1"`, `"id": " review-note-1 "`, 1)
		}, wantErr: "incomplete or invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := DecodeCandidateEnvelope([]byte(test.mutate(valid)))
			if CodeOf(err) != CodeValidation || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("DecodeCandidateEnvelope() error = %v, want validation containing %q", err, test.wantErr)
			}
		})
	}
}
