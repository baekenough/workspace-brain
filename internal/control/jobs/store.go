// Package jobs stores control-plane job state independently from the data core.
package jobs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// CoreStatusReader is the subset of the core contract needed for reconciliation.
type CoreStatusReader interface {
	JobStatus(ctx context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error)
}

// Clock returns the current time. It exists to make lifecycle tests deterministic.
type Clock func() time.Time

// Store is an in-memory control-plane job store.
type Store struct {
	mu    sync.RWMutex
	clock Clock
	jobs  map[string]brainapi.JobSnapshot
}

// Option configures a Store.
type Option func(*Store)

// WithClock injects a deterministic clock.
func WithClock(clock Clock) Option {
	return func(s *Store) {
		if clock != nil {
			s.clock = clock
		}
	}
}

// NewStore creates an empty job store.
func NewStore(opts ...Option) *Store {
	s := &Store{
		clock: time.Now,
		jobs:  make(map[string]brainapi.JobSnapshot),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// PutAccepted creates or replaces a job in accepted state.
func (s *Store) PutAccepted(tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error) {
	if err := validateIDs(tenantID, jobID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	return s.set(brainapi.JobSnapshot{TenantID: tenantID, JobID: jobID, Status: brainapi.JobAccepted})
}

// MarkRunning moves a job to running state.
func (s *Store) MarkRunning(tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error) {
	return s.update(tenantID, jobID, func(snapshot brainapi.JobSnapshot) brainapi.JobSnapshot {
		snapshot.Status = brainapi.JobRunning
		snapshot.Error = ""
		return snapshot
	})
}

// MarkCompleted moves a job to completed state.
func (s *Store) MarkCompleted(tenantID brainapi.TenantID, jobID brainapi.JobID, resultRef string) (brainapi.JobSnapshot, error) {
	return s.update(tenantID, jobID, func(snapshot brainapi.JobSnapshot) brainapi.JobSnapshot {
		snapshot.Status = brainapi.JobCompleted
		snapshot.ResultRef = resultRef
		snapshot.Error = ""
		return snapshot
	})
}

// MarkFailed moves a job to failed state.
func (s *Store) MarkFailed(tenantID brainapi.TenantID, jobID brainapi.JobID, message string) (brainapi.JobSnapshot, error) {
	return s.update(tenantID, jobID, func(snapshot brainapi.JobSnapshot) brainapi.JobSnapshot {
		snapshot.Status = brainapi.JobFailed
		snapshot.Error = message
		return snapshot
	})
}

// MarkCheckRequired records that callback/reconciliation could not prove a final state.
func (s *Store) MarkCheckRequired(tenantID brainapi.TenantID, jobID brainapi.JobID, message string) (brainapi.JobSnapshot, error) {
	return s.update(tenantID, jobID, func(snapshot brainapi.JobSnapshot) brainapi.JobSnapshot {
		snapshot.Status = brainapi.JobCheckRequired
		snapshot.Error = message
		return snapshot
	})
}

// Snapshot returns a tenant-scoped job snapshot.
func (s *Store) Snapshot(tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error) {
	if err := validateIDs(tenantID, jobID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.jobs[jobKey(tenantID, jobID)]
	if !ok {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindNotFound, "job_snapshot", "job not found", nil)
	}
	return snapshot, nil
}

// Reconcile fetches core status and stores it under the same tenant/job scope.
//
// If the core has no record of the job (brainapi.KindNotFound — e.g. the
// worker has not yet driven the job to completion, or the record was never
// created), Reconcile does not propagate the opaque not-found error to
// callers. Instead it falls back to reconcileNotFound: an already-final
// control-plane snapshot is preserved as-is, and a non-final (or missing)
// snapshot is mapped to [brainapi.JobCheckRequired] so callers can surface a
// clear "status unconfirmed" signal instead of a bare not-found.
func (s *Store) Reconcile(ctx context.Context, core CoreStatusReader, tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error) {
	if core == nil {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindInvalid, "job_reconcile", "core status reader is required", nil)
	}
	if err := validateIDs(tenantID, jobID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	snapshot, err := core.JobStatus(ctx, tenantID, jobID)
	if err != nil {
		if brainapi.IsKind(err, brainapi.KindNotFound) {
			return s.reconcileNotFound(tenantID, jobID, err)
		}
		return brainapi.JobSnapshot{}, err
	}
	if snapshot.TenantID != tenantID || snapshot.JobID != jobID {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindInternal, "job_reconcile", "core returned mismatched job scope", nil)
	}
	return s.set(snapshot)
}

// reconcileNotFound handles the core-not-found case for Reconcile.
//
//   - If the control-plane already holds a final snapshot (completed/failed),
//     it is preserved unchanged — a terminal state is never regressed by a
//     transient or stale core lookup miss.
//   - Otherwise the job is (re)marked [brainapi.JobCheckRequired] so status
//     surfaces (e.g. `/brain status`) can tell operators the state could not
//     be confirmed, rather than leaking a bare not-found error.
func (s *Store) reconcileNotFound(tenantID brainapi.TenantID, jobID brainapi.JobID, coreErr error) (brainapi.JobSnapshot, error) {
	s.mu.RLock()
	existing, ok := s.jobs[jobKey(tenantID, jobID)]
	s.mu.RUnlock()

	if ok && existing.Status.Final() {
		return existing, nil
	}

	message := fmt.Sprintf("core job record not found during reconcile: %v", coreErr)
	if ok {
		return s.MarkCheckRequired(tenantID, jobID, message)
	}
	return s.set(brainapi.JobSnapshot{TenantID: tenantID, JobID: jobID, Status: brainapi.JobCheckRequired, Error: message})
}

func (s *Store) set(snapshot brainapi.JobSnapshot) (brainapi.JobSnapshot, error) {
	if err := validateIDs(snapshot.TenantID, snapshot.JobID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	if snapshot.Status == "" {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindInvalid, "job_store", "status is required", nil)
	}
	snapshot.UpdatedAt = s.clock().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[jobKey(snapshot.TenantID, snapshot.JobID)] = snapshot
	return snapshot, nil
}

func (s *Store) update(tenantID brainapi.TenantID, jobID brainapi.JobID, fn func(brainapi.JobSnapshot) brainapi.JobSnapshot) (brainapi.JobSnapshot, error) {
	if err := validateIDs(tenantID, jobID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := jobKey(tenantID, jobID)
	snapshot, ok := s.jobs[key]
	if !ok {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindNotFound, "job_update", "job not found", nil)
	}
	snapshot = fn(snapshot)
	snapshot.UpdatedAt = s.clock().UTC()
	s.jobs[key] = snapshot
	return snapshot, nil
}

func validateIDs(tenantID brainapi.TenantID, jobID brainapi.JobID) error {
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateJobID(jobID); err != nil {
		return err
	}
	return nil
}

func jobKey(tenantID brainapi.TenantID, jobID brainapi.JobID) string {
	return string(tenantID) + "\x00" + string(jobID)
}
