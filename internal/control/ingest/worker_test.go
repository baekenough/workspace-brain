package ingest_test

import (
	"context"
	"sync"
	"testing"

	"github.com/sangyi/workspace-brain/internal/control/ingest"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// fakeCompleter records core CompleteJob calls.
type fakeCompleter struct {
	mu   sync.Mutex
	jobs []brainapi.JobID
}

func (f *fakeCompleter) CompleteJob(tenantID brainapi.TenantID, jobID brainapi.JobID, _ brainapi.JobStatus, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = tenantID
	f.jobs = append(f.jobs, jobID)
	return nil
}

func (f *fakeCompleter) completed() []brainapi.JobID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]brainapi.JobID, len(f.jobs))
	copy(out, f.jobs)
	return out
}

// fakeGW records gateway HandleJobCompleted calls.
type fakeGW struct {
	mu   sync.Mutex
	jobs []brainapi.JobID
}

func (g *fakeGW) HandleJobCompleted(_ context.Context, _ brainapi.TenantID, jobID brainapi.JobID, _ brainapi.JobStatus, _, _ string) (brainapi.JobSnapshot, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.jobs = append(g.jobs, jobID)
	return brainapi.JobSnapshot{}, nil
}

func (g *fakeGW) completed() []brainapi.JobID {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]brainapi.JobID, len(g.jobs))
	copy(out, g.jobs)
	return out
}

func TestWorkerCompletesJobOnEnqueue(t *testing.T) {
	t.Parallel()
	core := &fakeCompleter{}
	gw := &fakeGW{}
	w := ingest.NewWorker(core, gw, 8)
	w.Start()
	defer w.Stop()

	task := ingest.Task{TenantID: "tenant-1", JobID: "job-1"}
	if err := w.Enqueue(context.Background(), task); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	w.WaitForIdle()

	if got := core.completed(); len(got) != 1 || got[0] != "job-1" {
		t.Fatalf("core completed=%v", got)
	}
	if got := gw.completed(); len(got) != 1 || got[0] != "job-1" {
		t.Fatalf("gw completed=%v", got)
	}
}

func TestWorkerMultipleTasksAllComplete(t *testing.T) {
	t.Parallel()
	core := &fakeCompleter{}
	gw := &fakeGW{}
	w := ingest.NewWorker(core, gw, 16)
	w.Start()
	defer w.Stop()

	const n = 5
	ctx := context.Background()
	for i := range n {
		jobID := brainapi.JobID("job-" + string(rune('0'+i)))
		if err := w.Enqueue(ctx, ingest.Task{TenantID: "tenant-1", JobID: jobID}); err != nil {
			t.Fatalf("Enqueue[%d]: %v", i, err)
		}
	}

	w.WaitForIdle()

	if got := len(core.completed()); got != n {
		t.Fatalf("core completions=%d, want %d", got, n)
	}
	if got := len(gw.completed()); got != n {
		t.Fatalf("gw completions=%d, want %d", got, n)
	}
}

func TestWorkerEnqueueToStoppedWorkerReturnsError(t *testing.T) {
	t.Parallel()
	w := ingest.NewWorker(&fakeCompleter{}, &fakeGW{}, 8)
	// Stop without starting — no goroutine.
	w.Stop()

	err := w.Enqueue(context.Background(), ingest.Task{TenantID: "tenant-1", JobID: "job-1"})
	if err == nil {
		t.Fatal("expected error for stopped worker")
	}
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestWorkerEnqueueToFullQueueReturnsError(t *testing.T) {
	t.Parallel()
	// bufSize=1, no consumer goroutine (Start not called).
	w := ingest.NewWorker(&fakeCompleter{}, &fakeGW{}, 1)

	ctx := context.Background()
	// Fill the single-slot buffer.
	if err := w.Enqueue(ctx, ingest.Task{TenantID: "tenant-1", JobID: "job-1"}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// Second enqueue must fail (buffer full, no consumer).
	err := w.Enqueue(ctx, ingest.Task{TenantID: "tenant-1", JobID: "job-2"})
	if err == nil {
		t.Fatal("expected queue-full error")
	}
	if !brainapi.IsKind(err, brainapi.KindInternal) {
		t.Fatalf("kind=%q err=%v", brainapi.KindOf(err), err)
	}

	// Cleanup: start so the goroutine can drain and Stop can return.
	w.Start()
	w.Stop()
}

func TestWorkerStopIsIdempotent(t *testing.T) {
	t.Parallel()
	w := ingest.NewWorker(&fakeCompleter{}, &fakeGW{}, 8)
	w.Start()
	w.Stop()
	w.Stop() // second Stop must not panic
}

func TestWorkerDrainsBufferedTasksOnStop(t *testing.T) {
	t.Parallel()
	core := &fakeCompleter{}
	gw := &fakeGW{}
	// Large buffer; enqueue tasks before starting so they sit in the buffer.
	w := ingest.NewWorker(core, gw, 16)

	const n = 6
	ctx := context.Background()
	for i := range n {
		jobID := brainapi.JobID("job-" + string(rune('A'+i)))
		if err := w.Enqueue(ctx, ingest.Task{TenantID: "tenant-1", JobID: jobID}); err != nil {
			t.Fatalf("Enqueue[%d]: %v", i, err)
		}
	}

	w.Start()
	w.WaitForIdle()
	w.Stop()

	if got := len(core.completed()); got != n {
		t.Fatalf("core completions=%d, want %d", got, n)
	}
	if got := len(gw.completed()); got != n {
		t.Fatalf("gw completions=%d, want %d", got, n)
	}
}

func TestWorkerDefaultBufSizeAppliedForZero(t *testing.T) {
	t.Parallel()
	// NewWorker with bufSize=0 should not panic and should create a working worker.
	w := ingest.NewWorker(&fakeCompleter{}, &fakeGW{}, 0)
	w.Start()

	if err := w.Enqueue(context.Background(), ingest.Task{TenantID: "tenant-1", JobID: "job-1"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	w.WaitForIdle()
	w.Stop()
}

// TestWorkerStopDrainsBufferWithoutStart verifies that Stop() itself drains
// tasks that were buffered before Start() was called.  This exercises the
// post-wg.Wait drain loop inside Stop (the branch where the goroutine has
// not consumed the task but Stop still needs to process it).
func TestWorkerStopDrainsBufferWithoutStart(t *testing.T) {
	t.Parallel()
	core := &fakeCompleter{}
	gw := &fakeGW{}
	// Large buffer so Enqueue succeeds without a consumer goroutine.
	w := ingest.NewWorker(core, gw, 8)

	ctx := context.Background()
	for i := range 3 {
		jobID := brainapi.JobID("job-buf-" + string(rune('0'+i)))
		if err := w.Enqueue(ctx, ingest.Task{TenantID: "tenant-1", JobID: jobID}); err != nil {
			t.Fatalf("Enqueue[%d]: %v", i, err)
		}
	}

	// Stop without ever calling Start(): wg.Wait() returns immediately because
	// the goroutine was never launched.  The drain loop in Stop picks up the
	// buffered tasks.
	w.Stop()

	if got := len(core.completed()); got != 3 {
		t.Fatalf("core completions=%d, want 3", got)
	}
	if got := len(gw.completed()); got != 3 {
		t.Fatalf("gw completions=%d, want 3", got)
	}
}
