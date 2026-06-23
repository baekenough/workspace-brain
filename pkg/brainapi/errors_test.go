package brainapi

import (
	"errors"
	"testing"
)

func TestKindOf(t *testing.T) {
	cause := errors.New("boom")
	err := E(KindConflict, "op", "message", cause)
	if got := KindOf(err); got != KindConflict {
		t.Fatalf("KindOf() = %q, want %q", got, KindConflict)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("wrapped cause not preserved")
	}
	if got := KindOf(errors.New("plain")); got != KindInternal {
		t.Fatalf("plain errors should be internal, got %q", got)
	}
}

func TestSafeAccessError(t *testing.T) {
	err := SafeAccessError("ask")
	if !IsKind(err, KindUnauthorized) {
		t.Fatalf("SafeAccessError kind = %q", KindOf(err))
	}
	if got := err.Error(); got != "ask: unauthorized: project is not accessible" {
		t.Fatalf("safe message leaked or changed: %q", got)
	}
}

func TestErrorNilAndKeyHelpers(t *testing.T) {
	var e *Error
	if got := e.Error(); got != "<nil>" {
		t.Fatalf("nil Error() = %q", got)
	}
	if got := (Principal{ID: "U1"}).Key(); got != "U1" {
		t.Fatalf("principal key without source = %q", got)
	}
	p := Principal{Source: "slack", ID: "U1", Roles: []string{"Member"}}
	if got := p.Key(); got != "slack:U1" {
		t.Fatalf("principal key = %q", got)
	}
	if !p.HasRole("member") || p.HasRole("admin") {
		t.Fatalf("role lookup failed")
	}
	if !JobCompleted.Final() || !JobFailed.Final() || JobRunning.Final() {
		t.Fatalf("Final() classification failed")
	}
}

func TestKindOfNil(t *testing.T) {
	t.Parallel()
	if got := KindOf(nil); got != "" {
		t.Fatalf("KindOf(nil)=%q", got)
	}
}
