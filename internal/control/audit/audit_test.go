package audit_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/internal/control/audit"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestNoOpLoggerDiscardsAllEvents(t *testing.T) {
	t.Parallel()
	var l audit.NoOp
	// Must not panic and silently discard both allow and deny events.
	l.Log(audit.Event{
		Actor:    "slack:U1",
		Action:   brainapi.ActionQuery,
		TenantID: "tenant-1",
		Decision: audit.DecisionAllow,
	})
	l.Log(audit.Event{
		Actor:    "slack:U2",
		Action:   brainapi.ActionAdmin,
		TenantID: "tenant-2",
		Decision: audit.DecisionDeny,
		Reason:   "admin role required",
	})
}

func TestStructuredLoggerFormatsAllowEvent(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	l := audit.NewStructuredLogger(&buf, func() time.Time { return fixed })

	// Event with zero At: logger fills from its clock.
	l.Log(audit.Event{
		Actor:    "slack:U1",
		Action:   brainapi.ActionCreateProject,
		TenantID: "tenant-42",
		Decision: audit.DecisionAllow,
		Reason:   "",
		// At: zero — logger supplies the clock value
	})

	line := buf.String()
	for _, want := range []string{
		"2024-01-15T12:00:00Z",
		"slack:U1",
		string(brainapi.ActionCreateProject),
		"tenant-42",
		"allow",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q; got: %q", want, line)
		}
	}
}

func TestStructuredLoggerUsesNonZeroAt(t *testing.T) {
	t.Parallel()
	clockTime := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	providedTime := time.Date(2020, 3, 3, 9, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	l := audit.NewStructuredLogger(&buf, func() time.Time { return clockTime })

	// Non-zero At: logger must use the event's time, not the clock.
	l.Log(audit.Event{
		Actor:    "U2",
		Action:   brainapi.ActionAdmin,
		TenantID: "tenant-99",
		Decision: audit.DecisionDeny,
		Reason:   "not admin",
		At:       providedTime,
	})

	line := buf.String()
	if !strings.Contains(line, "2020-03-03T09:00:00Z") {
		t.Errorf("should use provided At timestamp; got: %q", line)
	}
	if strings.Contains(line, "2024-06-01") {
		t.Errorf("should NOT use clock time; got: %q", line)
	}
	if !strings.Contains(line, "deny") {
		t.Errorf("missing decision=deny; got: %q", line)
	}
	if !strings.Contains(line, "not admin") {
		t.Errorf("missing reason; got: %q", line)
	}
}

func TestNewStructuredLoggerNilClockDefaultsToTimeNow(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := audit.NewStructuredLogger(&buf, nil) // nil => uses time.Now
	l.Log(audit.Event{
		Actor:    "U",
		Action:   brainapi.ActionQuery,
		TenantID: "t",
		Decision: audit.DecisionAllow,
	})
	if buf.Len() == 0 {
		t.Error("expected non-empty output with nil clock")
	}
}

func TestDecisionConstants(t *testing.T) {
	t.Parallel()
	if audit.DecisionAllow != "allow" {
		t.Errorf("DecisionAllow = %q", audit.DecisionAllow)
	}
	if audit.DecisionDeny != "deny" {
		t.Errorf("DecisionDeny = %q", audit.DecisionDeny)
	}
}
