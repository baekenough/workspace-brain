package jobs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

func TestStoreLifecycle(t *testing.T) {
	t.Parallel()
	current := time.Date(2026, 6, 23, 1, 2, 3, 0, time.FixedZone("KST", 9*60*60))
	store := NewStore(WithClock(func() time.Time { return current }))

	accepted, err := store.PutAccepted("tenant-a", "job-1")
	if err != nil {
		t.Fatalf("PutAccepted: %v", err)
	}
	if accepted.Status != brainapi.JobAccepted || !accepted.UpdatedAt.Equal(current.UTC()) {
		t.Fatalf("accepted snapshot = %+v", accepted)
	}

	current = current.Add(time.Minute)
	running, err := store.MarkRunning("tenant-a", "job-1")
	if err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if running.Status != brainapi.JobRunning || !running.UpdatedAt.Equal(current.UTC()) {
		t.Fatalf("running snapshot = %+v", running)
	}

	current = current.Add(time.Minute)
	completed, err := store.MarkCompleted("tenant-a", "job-1", "bronze://result")
	if err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	if completed.Status != brainapi.JobCompleted || completed.ResultRef != "bronze://result" || !completed.Status.Final() {
		t.Fatalf("completed snapshot = %+v", completed)
	}
}

func TestStoreTenantIsolation(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if _, err := store.PutAccepted("tenant-a", "same-job"); err != nil {
		t.Fatalf("PutAccepted tenant-a: %v", err)
	}
	if _, err := store.PutAccepted("tenant-b", "same-job"); err != nil {
		t.Fatalf("PutAccepted tenant-b: %v", err)
	}
	if _, err := store.MarkCompleted("tenant-a", "same-job", "result-a"); err != nil {
		t.Fatalf("MarkCompleted tenant-a: %v", err)
	}
	b, err := store.Snapshot("tenant-b", "same-job")
	if err != nil {
		t.Fatalf("Snapshot tenant-b: %v", err)
	}
	if b.Status != brainapi.JobAccepted || b.ResultRef != "" {
		t.Fatalf("tenant-b leaked tenant-a result: %+v", b)
	}
}

func TestStoreRejectsMissingJob(t *testing.T) {
	t.Parallel()
	store := NewStore()
	_, err := store.MarkRunning("tenant-a", "missing")
	if !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("MarkRunning missing kind = %q err=%v", brainapi.KindOf(err), err)
	}
}

func TestStoreReconcileChecksScope(t *testing.T) {
	t.Parallel()
	store := NewStore()
	core := statusReaderFunc(func(context.Context, brainapi.TenantID, brainapi.JobID) (brainapi.JobSnapshot, error) {
		return brainapi.JobSnapshot{TenantID: "other", JobID: "job-1", Status: brainapi.JobCompleted}, nil
	})
	_, err := store.Reconcile(context.Background(), core, "tenant-a", "job-1")
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("Reconcile mismatched scope kind = %q err=%v", brainapi.KindOf(err), err)
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	store := NewStore()
	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenantID := brainapi.TenantID("tenant")
			jobID := brainapi.JobID("job-" + string(rune('a'+i%26)))
			_, _ = store.PutAccepted(tenantID, jobID)
			_, _ = store.MarkRunning(tenantID, jobID)
			_, _ = store.Snapshot(tenantID, jobID)
		}(i)
	}
	wg.Wait()
}

type statusReaderFunc func(context.Context, brainapi.TenantID, brainapi.JobID) (brainapi.JobSnapshot, error)

func (f statusReaderFunc) JobStatus(ctx context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error) {
	return f(ctx, tenantID, jobID)
}

func TestStoreFailureAndCheckRequiredStates(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if _, err := store.PutAccepted("tenant-a", "job-1"); err != nil {
		t.Fatalf("PutAccepted: %v", err)
	}
	failed, err := store.MarkFailed("tenant-a", "job-1", "boom")
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if failed.Status != brainapi.JobFailed || failed.Error != "boom" {
		t.Fatalf("failed = %+v", failed)
	}
	check, err := store.MarkCheckRequired("tenant-a", "job-1", "unknown")
	if err != nil {
		t.Fatalf("MarkCheckRequired: %v", err)
	}
	if check.Status != brainapi.JobCheckRequired || check.Error != "unknown" {
		t.Fatalf("check = %+v", check)
	}
}

func TestStoreReconcileSuccessAndNilCore(t *testing.T) {
	t.Parallel()
	store := NewStore()
	_, err := store.Reconcile(context.Background(), nil, "tenant-a", "job-1")
	if !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("nil core kind=%q err=%v", brainapi.KindOf(err), err)
	}
	core := statusReaderFunc(func(context.Context, brainapi.TenantID, brainapi.JobID) (brainapi.JobSnapshot, error) {
		return brainapi.JobSnapshot{TenantID: "tenant-a", JobID: "job-1", Status: brainapi.JobCompleted, ResultRef: "done"}, nil
	})
	snapshot, err := store.Reconcile(context.Background(), core, "tenant-a", "job-1")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if snapshot.Status != brainapi.JobCompleted || snapshot.ResultRef != "done" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestStoreValidationBranches(t *testing.T) {
	t.Parallel()
	store := NewStore()
	if _, err := store.PutAccepted("bad tenant", "job-1"); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("put bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := store.PutAccepted("tenant-a", "bad job"); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("put bad job kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := store.Snapshot("bad tenant", "job-1"); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("snapshot bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := store.Snapshot("tenant-a", "missing"); !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("snapshot missing kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := store.Reconcile(context.Background(), statusReaderFunc(func(context.Context, brainapi.TenantID, brainapi.JobID) (brainapi.JobSnapshot, error) {
		return brainapi.JobSnapshot{}, nil
	}), "bad tenant", "job-1"); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("reconcile bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := store.set(brainapi.JobSnapshot{TenantID: "tenant-a", JobID: "job-1"}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("set missing status kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := store.update("bad tenant", "job-1", func(s brainapi.JobSnapshot) brainapi.JobSnapshot { return s }); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("update bad tenant kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestStoreReconcileNotFoundFallback(t *testing.T) {
	t.Parallel()
	notFound := statusReaderFunc(func(context.Context, brainapi.TenantID, brainapi.JobID) (brainapi.JobSnapshot, error) {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindNotFound, "job_status", "job not found", nil)
	})

	t.Run("no local snapshot creates check-required", func(t *testing.T) {
		t.Parallel()
		store := NewStore()
		snapshot, err := store.Reconcile(context.Background(), notFound, "tenant-a", "job-new")
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if snapshot.Status != brainapi.JobCheckRequired || snapshot.Error == "" {
			t.Fatalf("snapshot = %+v, want check_required with a message", snapshot)
		}
		// The fallback snapshot must actually be persisted so a subsequent
		// Snapshot() call observes the same check-required state.
		stored, err := store.Snapshot("tenant-a", "job-new")
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if stored.Status != brainapi.JobCheckRequired {
			t.Fatalf("stored = %+v, want check_required", stored)
		}
	})

	t.Run("non-final local snapshot is marked check-required", func(t *testing.T) {
		t.Parallel()
		store := NewStore()
		if _, err := store.PutAccepted("tenant-a", "job-running"); err != nil {
			t.Fatalf("PutAccepted: %v", err)
		}
		if _, err := store.MarkRunning("tenant-a", "job-running"); err != nil {
			t.Fatalf("MarkRunning: %v", err)
		}
		snapshot, err := store.Reconcile(context.Background(), notFound, "tenant-a", "job-running")
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if snapshot.Status != brainapi.JobCheckRequired || snapshot.Error == "" {
			t.Fatalf("snapshot = %+v, want check_required with a message", snapshot)
		}
	})

	t.Run("final local snapshot is preserved unchanged", func(t *testing.T) {
		t.Parallel()
		store := NewStore()
		if _, err := store.PutAccepted("tenant-a", "job-done"); err != nil {
			t.Fatalf("PutAccepted: %v", err)
		}
		completed, err := store.MarkCompleted("tenant-a", "job-done", "bronze://result")
		if err != nil {
			t.Fatalf("MarkCompleted: %v", err)
		}
		snapshot, err := store.Reconcile(context.Background(), notFound, "tenant-a", "job-done")
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if snapshot != completed {
			t.Fatalf("snapshot = %+v, want unchanged final snapshot %+v", snapshot, completed)
		}
	})
}

func TestStoreRemainingReconcileAndSetErrors(t *testing.T) {
	t.Parallel()
	store := NewStore()
	coreErr := statusReaderFunc(func(context.Context, brainapi.TenantID, brainapi.JobID) (brainapi.JobSnapshot, error) {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindInternal, "core", "down", nil)
	})
	if _, err := store.Reconcile(context.Background(), coreErr, "tenant-a", "job-1"); !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("reconcile core err kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if _, err := store.set(brainapi.JobSnapshot{TenantID: "bad tenant", JobID: "job-1", Status: brainapi.JobRunning}); !brainapi.IsKind(err, brainapi.KindInvalid) {
		t.Fatalf("set bad ids kind=%q err=%v", brainapi.KindOf(err), err)
	}
}
