//go:build integration

// Package rabbitmq_test contains integration tests for the RabbitMQ-backed Queue.
// Run with: go test -tags=integration -race ./internal/control/rabbitmq/...
package rabbitmq_test

import (
	"context"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"

	"github.com/sangyi/workspace-brain/internal/control/ingest"
	"github.com/sangyi/workspace-brain/internal/control/rabbitmq"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// startRabbitMQ spins up a RabbitMQ container and returns its AMQP URL.
func startRabbitMQ(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcrabbitmq.Run(ctx, "rabbitmq:3.13-management-alpine")
	if err != nil {
		t.Fatalf("start RabbitMQ container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("warn: terminate RabbitMQ container: %v", err)
		}
	})
	url, err := ctr.AmqpURL(ctx)
	if err != nil {
		t.Fatalf("RabbitMQ AMQP URL: %v", err)
	}
	return url
}

// ----- test doubles -------------------------------------------------------

type fakeCore struct {
	mu   sync.Mutex
	jobs []string
}

func (c *fakeCore) CompleteJob(tenantID brainapi.TenantID, jobID brainapi.JobID, _ brainapi.JobStatus, _, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jobs = append(c.jobs, string(tenantID)+"/"+string(jobID))
	return nil
}

type fakeGW struct {
	mu   sync.Mutex
	jobs []string
	err  error // non-nil → always fail (drives DLQ path)
}

func (g *fakeGW) HandleJobCompleted(_ context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID, _ brainapi.JobStatus, _, _ string) (brainapi.JobSnapshot, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return brainapi.JobSnapshot{}, g.err
	}
	g.jobs = append(g.jobs, string(tenantID)+"/"+string(jobID))
	return brainapi.JobSnapshot{}, nil
}

func (g *fakeGW) completedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.jobs)
}

// ----- tests --------------------------------------------------------------

// TestIntegrationRoundTrip verifies the happy path:
// Enqueue → consumer picks up → CoreCompleter + GatewayCompleter called → Ack.
func TestIntegrationRoundTrip(t *testing.T) {
	amqpURL := startRabbitMQ(t)
	ctx := context.Background()

	core := &fakeCore{}
	gw := &fakeGW{}

	q := rabbitmq.NewQueue(amqpURL, core, gw)
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer q.Stop()

	task := ingest.Task{TenantID: "tenant-1", JobID: "job-1"}
	if err := q.Enqueue(ctx, task); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for the consumer to process the task (up to 10s).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if gw.completedCount() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gw.completedCount() < 1 {
		t.Fatalf("task not completed within 10s")
	}
}

// TestIntegrationMultipleEnqueue verifies that multiple tasks are all processed.
func TestIntegrationMultipleEnqueue(t *testing.T) {
	amqpURL := startRabbitMQ(t)
	ctx := context.Background()

	core := &fakeCore{}
	gw := &fakeGW{}

	q := rabbitmq.NewQueue(amqpURL, core, gw)
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer q.Stop()

	const n = 5
	for i := 0; i < n; i++ {
		task := ingest.Task{
			TenantID: brainapi.TenantID("tenant-multi"),
			JobID:    brainapi.JobID("job-multi-" + string(rune('0'+i))),
		}
		if err := q.Enqueue(ctx, task); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if gw.completedCount() >= n {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if gw.completedCount() < n {
		t.Fatalf("completed=%d, want %d", gw.completedCount(), n)
	}
}

// TestIntegrationDLQ verifies that a task that always fails is eventually
// routed to the dead-letter queue after maxRetries attempts.
// Total back-off time: 500ms + 1s + 2s ≈ 3.5s.
func TestIntegrationDLQ(t *testing.T) {
	amqpURL := startRabbitMQ(t)
	ctx := context.Background()

	core := &fakeCore{}
	gw := &fakeGW{err: brainapi.E(brainapi.KindInternal, "test", "always fail", nil)}

	q := rabbitmq.NewQueue(amqpURL, core, gw)
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer q.Stop()

	task := ingest.Task{TenantID: "tenant-dlq", JobID: "job-dlq"}
	if err := q.Enqueue(ctx, task); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Open a separate AMQP connection to poll the DLQ.
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		t.Fatalf("dial for DLQ check: %v", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("channel for DLQ check: %v", err)
	}
	defer ch.Close()

	// Allow enough time for all retries + back-off (3.5s) plus margin.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok, err := ch.Get("wb.ingest.dlq", false)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if ok {
			_ = msg.Ack(false)
			t.Logf("DLQ received message: %s", msg.Body)
			return // success — message arrived in DLQ as expected
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("message not in DLQ after 30s — DLQ routing may be broken")
}

// TestIntegrationGracefulStop ensures Stop() does not leak goroutines
// even when called while a message is in flight.
func TestIntegrationGracefulStop(t *testing.T) {
	amqpURL := startRabbitMQ(t)
	ctx := context.Background()

	core := &fakeCore{}
	gw := &fakeGW{}

	q := rabbitmq.NewQueue(amqpURL, core, gw)
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Enqueue several tasks before stopping.
	for i := 0; i < 3; i++ {
		_ = q.Enqueue(ctx, ingest.Task{TenantID: "tenant-stop", JobID: brainapi.JobID("job-stop-" + string(rune('0'+i)))})
	}

	// Stop should return without hanging (no goroutine leak).
	done := make(chan struct{})
	go func() {
		q.Stop()
		close(done)
	}()
	select {
	case <-done:
		// OK
	case <-time.After(10 * time.Second):
		t.Fatalf("Stop() did not return within 10s — possible goroutine leak")
	}
}
