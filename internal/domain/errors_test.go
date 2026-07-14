package domain

import (
	"errors"
	"testing"
)

func TestObjectMissingErrorCodeSurvivesWrapping(t *testing.T) {
	err := NewError(CodeObjectMissing, errors.New("object is missing"))

	if got := CodeOf(err); got != CodeObjectMissing {
		t.Fatalf("CodeOf() = %q, want %q", got, CodeObjectMissing)
	}
}
