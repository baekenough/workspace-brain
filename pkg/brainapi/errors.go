// Package brainapi defines the surface-neutral contract shared by workspace-brain's
// control plane and data core.
package brainapi

import (
	"errors"
	"fmt"
)

// ErrorKind classifies contract errors without leaking implementation details.
type ErrorKind string

const (
	// KindInvalid means the caller supplied malformed or incomplete input.
	KindInvalid ErrorKind = "invalid"
	// KindNotFound means the requested tenant-scoped resource does not exist.
	KindNotFound ErrorKind = "not_found"
	// KindAlreadyExists means a create operation would duplicate an existing resource.
	KindAlreadyExists ErrorKind = "already_exists"
	// KindUnauthorized means the resolved principal cannot perform the requested action.
	KindUnauthorized ErrorKind = "unauthorized"
	// KindConflict means the request is well-formed but conflicts with current state.
	KindConflict ErrorKind = "conflict"
	// KindInternal means the contract boundary observed an unexpected failure.
	KindInternal ErrorKind = "internal"
)

// Error is a typed contract error.
type Error struct {
	Kind ErrorKind
	Op   string
	Msg  string
	Err  error
}

// Error returns a concise, operation-scoped message.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	base := string(e.Kind)
	if e.Op != "" {
		base = e.Op + ": " + base
	}
	if e.Msg != "" {
		base += ": " + e.Msg
	}
	return base
}

// Unwrap returns the wrapped cause.
func (e *Error) Unwrap() error { return e.Err }

// E creates a typed contract error.
func E(kind ErrorKind, op, msg string, err error) error {
	return &Error{Kind: kind, Op: op, Msg: msg, Err: err}
}

// KindOf extracts an ErrorKind from err. Unknown errors are internal.
func KindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// IsKind reports whether err has the provided kind.
func IsKind(err error, kind ErrorKind) bool {
	return KindOf(err) == kind
}

// SafeAccessError hides binding existence and authorization differences from surfaces.
func SafeAccessError(op string) error {
	return E(KindUnauthorized, op, "project is not accessible", nil)
}

func invalidf(op, format string, args ...any) error {
	return E(KindInvalid, op, fmt.Sprintf(format, args...), nil)
}
