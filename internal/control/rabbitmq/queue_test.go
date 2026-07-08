package rabbitmq

// White-box unit tests for the handleDelivery/retryOrDLQ error paths. These
// run under the default build (no RabbitMQ broker required) by driving
// handleDelivery/retryOrDLQ directly against a fake amqp.Acknowledger and fake
// CoreCompleter/GatewayCompleter doubles. Broker-dependent behavior (actual
// publish/consume round-trips, DLQ delivery via the real DLX) remains covered
// by the //go:build integration tests in queue_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/sangyi/workspace-brain/internal/control/ingest"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// fakeAcknowledger is a test double for amqp.Acknowledger — it records
// Ack/Nack calls so tests can assert on delivery outcome without a real
// broker connection.
type fakeAcknowledger struct {
	mu      sync.Mutex
	acked   int
	nacked  int
	nackReq bool // requeue flag from the most recent Nack call
}

func (f *fakeAcknowledger) Ack(_ uint64, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acked++
	return nil
}

func (f *fakeAcknowledger) Nack(_ uint64, _ bool, requeue bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nacked++
	f.nackReq = requeue
	return nil
}

func (f *fakeAcknowledger) Reject(_ uint64, _ bool) error { return nil }

// completerCall records a single CompleteJob/HandleJobCompleted invocation.
type completerCall struct {
	tenantID brainapi.TenantID
	jobID    brainapi.JobID
	status   brainapi.JobStatus
	message  string
}

// fakeCompleter is a shared test double satisfying both ingest.CoreCompleter
// and ingest.GatewayCompleter. When err is set, HandleJobCompleted returns it
// so tests can deterministically drive the retry/DLQ path.
type fakeCompleter struct {
	mu    sync.Mutex
	calls []completerCall
	err   error
}

func (f *fakeCompleter) CompleteJob(tenantID brainapi.TenantID, jobID brainapi.JobID, status brainapi.JobStatus, _ string, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, completerCall{tenantID: tenantID, jobID: jobID, status: status, message: message})
	return nil
}

func (f *fakeCompleter) HandleJobCompleted(_ context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID, status brainapi.JobStatus, _ string, message string) (brainapi.JobSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, completerCall{tenantID: tenantID, jobID: jobID, status: status, message: message})
	if f.err != nil {
		return brainapi.JobSnapshot{}, f.err
	}
	return brainapi.JobSnapshot{TenantID: tenantID, JobID: jobID, Status: status}, nil
}

func (f *fakeCompleter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeCompleter) lastCall() completerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func TestHandleDelivery_UnmarshalFailure_RoutesToDLQWithoutCompleterCalls(t *testing.T) {
	t.Parallel()
	core := &fakeCompleter{}
	gw := &fakeCompleter{}
	q := NewQueue("amqp://unused", core, gw)

	ack := &fakeAcknowledger{}
	d := amqp.Delivery{Acknowledger: ack, Body: []byte("not-json")}

	q.handleDelivery(d)

	if ack.nacked != 1 || ack.acked != 0 {
		t.Fatalf("ack=%d nack=%d, want ack=0 nack=1", ack.acked, ack.nacked)
	}
	if ack.nackReq {
		t.Fatalf("nack requeue = true, want false (routes to DLQ)")
	}
	// Task context is unrecoverable before unmarshal succeeds, so no
	// job-state transition can (or should) be attempted.
	if core.callCount() != 0 || gw.callCount() != 0 {
		t.Fatalf("completers should not be called for unmarshal failures: core=%d gw=%d", core.callCount(), gw.callCount())
	}
}

func TestHandleDelivery_Success_AcksAndCompletesJob(t *testing.T) {
	t.Parallel()
	core := &fakeCompleter{}
	gw := &fakeCompleter{}
	q := NewQueue("amqp://unused", core, gw)

	task := ingest.Task{TenantID: "tenant-a", JobID: "job-1"}
	body, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	ack := &fakeAcknowledger{}
	d := amqp.Delivery{Acknowledger: ack, Body: body}

	q.handleDelivery(d)

	if ack.acked != 1 || ack.nacked != 0 {
		t.Fatalf("ack=%d nack=%d, want ack=1 nack=0", ack.acked, ack.nacked)
	}
	if got := gw.lastCall(); got.status != brainapi.JobCompleted || got.tenantID != task.TenantID || got.jobID != task.JobID {
		t.Fatalf("gw call = %+v, want completed for %+v", got, task)
	}
}

func TestHandleDelivery_MaxRetriesExceeded_MarksJobFailedAndRoutesToDLQ(t *testing.T) {
	t.Parallel()
	core := &fakeCompleter{}
	gw := &fakeCompleter{err: brainapi.E(brainapi.KindInternal, "test", "always fail", nil)}
	q := NewQueue("amqp://unused", core, gw)

	task := ingest.Task{TenantID: "tenant-dlq", JobID: "job-dlq"}
	body, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	ack := &fakeAcknowledger{}
	d := amqp.Delivery{
		Acknowledger: ack,
		Body:         body,
		Headers:      amqp.Table{headerRetry: maxRetries}, // retries already exhausted
	}

	q.handleDelivery(d)

	if ack.nacked != 1 || ack.acked != 0 {
		t.Fatalf("ack=%d nack=%d, want ack=0 nack=1 (DLQ)", ack.acked, ack.nacked)
	}
	if ack.nackReq {
		t.Fatalf("nack requeue = true, want false (routes to DLQ)")
	}

	// Both completers must be driven to JobFailed so the control-plane job
	// status converges with the DLQ-routed message instead of being stuck in
	// its last non-terminal state (issue G6, item 2).
	if got := core.lastCall(); got.status != brainapi.JobFailed || got.tenantID != task.TenantID || got.jobID != task.JobID {
		t.Fatalf("core call = %+v, want failed for %+v", got, task)
	}
	if got := gw.lastCall(); got.status != brainapi.JobFailed || got.tenantID != task.TenantID || got.jobID != task.JobID {
		t.Fatalf("gw call = %+v, want failed for %+v", got, task)
	}
}

func TestRetryOrDLQ_StopSignalDuringBackoff_MarksJobFailedAndRoutesToDLQ(t *testing.T) {
	t.Parallel()
	core := &fakeCompleter{}
	gw := &fakeCompleter{}
	q := NewQueue("amqp://unused", core, gw)
	close(q.stopCh) // simulate Stop() having been called while a retry is pending

	task := ingest.Task{TenantID: "tenant-stop", JobID: "job-stop"}
	ack := &fakeAcknowledger{}
	d := amqp.Delivery{Acknowledger: ack, Headers: amqp.Table{headerRetry: int32(0)}}

	q.retryOrDLQ(d, task, errors.New("boom"))

	if ack.nacked != 1 || ack.acked != 0 {
		t.Fatalf("ack=%d nack=%d, want ack=0 nack=1", ack.acked, ack.nacked)
	}
	if got := core.lastCall(); got.status != brainapi.JobFailed || got.tenantID != task.TenantID || got.jobID != task.JobID {
		t.Fatalf("core call = %+v, want failed for %+v", got, task)
	}
	if got := gw.lastCall(); got.status != brainapi.JobFailed || got.tenantID != task.TenantID || got.jobID != task.JobID {
		t.Fatalf("gw call = %+v, want failed for %+v", got, task)
	}
}
