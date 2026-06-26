// Package ingest provides the async job-completion seam for the ingest pipeline.
//
// The in-process [Worker] receives lightweight completion tasks after the
// gateway has already indexed content synchronously via core.Ingest.  The
// worker then drives each job to a terminal state by calling
// CoreCompleter.CompleteJob (core-side) and GatewayCompleter.HandleJobCompleted
// (control-plane store).
//
// WB-06b can replace the in-process Queue implementation with a
// RabbitMQ-backed one without touching gateway.Gateway — the same seam pattern
// used by Embedder and VectorStore in the data core.
package ingest

import (
	"context"
	"sync"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// Queue is the async work-acceptance seam.
// Implementations must be safe for concurrent use and must return quickly
// (non-blocking accept or reject).
type Queue interface {
	// Enqueue schedules a completion task for the given job.
	// It returns an error if the worker is stopped or the queue is full.
	Enqueue(ctx context.Context, task Task) error
}

// Task holds the tenant/job identifiers needed to finalize an ingest job.
// Content indexing has already completed synchronously before the task is
// enqueued; the worker only updates job-state bookkeeping.
type Task struct {
	TenantID brainapi.TenantID
	JobID    brainapi.JobID
}

// CoreCompleter marks a core-side job terminal.
// *memory.Core satisfies this interface via its CompleteJob method.
type CoreCompleter interface {
	CompleteJob(tenantID brainapi.TenantID, jobID brainapi.JobID, status brainapi.JobStatus, resultRef, message string) error
}

// GatewayCompleter marks a control-plane job terminal.
// *gateway.Gateway satisfies this interface via HandleJobCompleted.
type GatewayCompleter interface {
	HandleJobCompleted(ctx context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID, status brainapi.JobStatus, resultRef string, message string) (brainapi.JobSnapshot, error)
}

// Worker is an in-process ingest-completion worker.
// It satisfies [Queue].  Use [NewWorker] to construct, [Start] to begin
// consuming tasks, and [Stop] to drain and shut down cleanly.
type Worker struct {
	core     CoreCompleter
	gw       GatewayCompleter
	tasks    chan Task
	wg       sync.WaitGroup // goroutine lifecycle
	inflight sync.WaitGroup // in-flight task tracking (for WaitForIdle)
	stopped  chan struct{}
	stopOnce sync.Once
}

// NewWorker creates a Worker with the given buffer size.
// bufSize <= 0 defaults to 64.
func NewWorker(core CoreCompleter, gw GatewayCompleter, bufSize int) *Worker {
	if bufSize <= 0 {
		bufSize = 64
	}
	return &Worker{
		core:    core,
		gw:      gw,
		tasks:   make(chan Task, bufSize),
		stopped: make(chan struct{}),
	}
}

// Enqueue schedules a completion task. It satisfies [Queue].
//
// Enqueue returns an error if the worker has been stopped or the internal
// buffer is full.  It must not be called concurrently with [Stop].
func (w *Worker) Enqueue(_ context.Context, task Task) error {
	select {
	case <-w.stopped:
		return brainapi.E(brainapi.KindInternal, "ingest_enqueue", "worker is stopped", nil)
	default:
	}
	w.inflight.Add(1)
	select {
	case w.tasks <- task:
		return nil
	default:
		w.inflight.Done()
		return brainapi.E(brainapi.KindInternal, "ingest_enqueue", "ingest queue is full", nil)
	}
}

// Start launches the background processing goroutine.
// It must be called exactly once before any [Enqueue] call.
func (w *Worker) Start() {
	w.wg.Add(1)
	go w.run()
}

// Stop signals the worker to stop accepting new tasks and blocks until the
// background goroutine has exited.  It then drains any tasks that remain in
// the buffer — including tasks enqueued after the goroutine noticed stopped —
// so that all inflight WaitGroup additions are matched by Done calls.
// Stop is safe to call multiple times.
func (w *Worker) Stop() {
	w.stopOnce.Do(func() { close(w.stopped) })
	w.wg.Wait()
	// Drain any tasks still buffered after the goroutine exited.
	for {
		select {
		case task := <-w.tasks:
			w.process(task)
		default:
			return
		}
	}
}

// WaitForIdle blocks until all tasks enqueued so far have been processed.
// It is intended for use in tests to avoid time.Sleep-based waits.
func (w *Worker) WaitForIdle() {
	w.inflight.Wait()
}

func (w *Worker) run() {
	defer w.wg.Done()
	for {
		select {
		case task := <-w.tasks:
			w.process(task)
		case <-w.stopped:
			// Exit immediately; Stop() drains any remaining buffered tasks
			// after wg.Wait() returns.
			return
		}
	}
}

func (w *Worker) process(task Task) {
	defer w.inflight.Done()
	ctx := context.Background()
	// Update the core-side job record (best-effort — ignore error because
	// a missing core record should not block the control-plane update).
	_ = w.core.CompleteJob(task.TenantID, task.JobID, brainapi.JobCompleted, "", "")
	// Update the control-plane store so Status() reflects the terminal state.
	_, _ = w.gw.HandleJobCompleted(ctx, task.TenantID, task.JobID, brainapi.JobCompleted, "", "")
}
