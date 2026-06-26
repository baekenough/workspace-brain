// Package audit provides the audit-event model for authorization-relevant actions
// in the workspace-brain control plane.
//
// The primary extension point is the Logger interface, which the gateway accepts
// via WithAuditLogger. The default is NoOp; callers that want a structured log
// stream pass a StructuredLogger (or their own implementation).
package audit

import (
	"fmt"
	"io"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// Decision indicates whether an authorization check allowed or denied the action.
type Decision string

const (
	// DecisionAllow means the principal was permitted to perform the action.
	DecisionAllow Decision = "allow"
	// DecisionDeny means the principal was rejected.
	DecisionDeny Decision = "deny"
)

// Event is an immutable record of a single authorization decision.
type Event struct {
	// Actor is the principal key (see brainapi.Principal.Key).
	Actor string
	// Action is the gateway action that was checked.
	Action brainapi.Action
	// TenantID is the resolved tenant scope; may be empty for pre-resolution events.
	TenantID brainapi.TenantID
	// Decision is the outcome of the authorization check.
	Decision Decision
	// Reason carries the denial message; empty when Decision is DecisionAllow.
	Reason string
	// At is the event timestamp. If zero, the Logger fills it from its own clock.
	At time.Time
}

// Logger emits authorization audit events. Implementations must be safe for
// concurrent use.
type Logger interface {
	Log(e Event)
}

// NoOp silently discards all events. It is the zero-value gateway logger so
// that callers who do not supply an audit logger see no behavioral change.
type NoOp struct{}

// Log implements Logger as a deliberate no-op.
func (NoOp) Log(Event) {}

// StructuredLogger writes events to w in a line-oriented key=value format.
// It uses now to timestamp events whose At field is zero.
type StructuredLogger struct {
	w   io.Writer
	now func() time.Time
}

// NewStructuredLogger returns a StructuredLogger that writes to w and uses now
// to fill zero At fields. If now is nil, time.Now is used.
func NewStructuredLogger(w io.Writer, now func() time.Time) *StructuredLogger {
	if now == nil {
		now = time.Now
	}
	return &StructuredLogger{w: w, now: now}
}

// Log writes a single structured audit line to w.
// If e.At is zero, it is filled from the logger's clock before formatting.
func (l *StructuredLogger) Log(e Event) {
	at := e.At
	if at.IsZero() {
		at = l.now()
	}
	fmt.Fprintf(l.w, "at=%s actor=%q action=%s tenant=%s decision=%s reason=%q\n",
		at.UTC().Format(time.RFC3339),
		e.Actor,
		e.Action,
		e.TenantID,
		e.Decision,
		e.Reason,
	)
}
