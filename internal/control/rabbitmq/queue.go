// Package rabbitmq provides a RabbitMQ-backed implementation of ingest.Queue.
//
// Topology
//
//	Exchange:  wb.ingest  (direct, durable)
//	Queue:     wb.ingest.tasks  (durable, x-dead-letter-exchange → wb.ingest.dlx)
//	Routing:   ingest.task
//	DLX:       wb.ingest.dlx  (direct, durable)
//	DLQ:       wb.ingest.dlq  (durable, bound to wb.ingest.dlx with key ingest.dlq)
//
// Publisher confirms are enabled; [Queue.Enqueue] waits for the broker ACK before
// returning.  Failed tasks are retried up to [maxRetries] times with exponential
// back-off (500ms, 1s, 2s), then nacked without requeue so the broker routes
// them to the DLQ via the DLX.
package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/sangyi/workspace-brain/internal/control/ingest"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// Topology constants — all names are prefixed with wb. to avoid collisions with
// other applications sharing the same RabbitMQ broker.
const (
	mainExchange  = "wb.ingest"
	mainQueue     = "wb.ingest.tasks"
	mainRouteKey  = "ingest.task"
	dlxExchange   = "wb.ingest.dlx"
	dlqQueue      = "wb.ingest.dlq"
	dlqRouteKey   = "ingest.dlq"
	headerRetry   = "x-wb-retry-count"
	maxRetries    = int32(3)
	retryBaseWait = 500 * time.Millisecond
)

// Queue is a RabbitMQ-backed ingest.Queue.
// Use [NewQueue] to construct, [Queue.Start] to connect and begin consuming,
// and [Queue.Stop] to drain in-flight work and close the connection gracefully.
type Queue struct {
	url      string
	core     ingest.CoreCompleter
	gw       ingest.GatewayCompleter
	conn     *amqp.Connection
	pubCh    *amqp.Channel
	consCh   *amqp.Channel
	confirms chan amqp.Confirmation
	pubMu    sync.Mutex // serialises publish + confirm-wait pairs
	stopCh   chan struct{}
	once     sync.Once
	wg       sync.WaitGroup
}

// NewQueue creates a new, unconnected Queue.
// Call [Queue.Start] to establish the AMQP connection and begin consuming.
func NewQueue(url string, core ingest.CoreCompleter, gw ingest.GatewayCompleter) *Queue {
	return &Queue{
		url:    url,
		core:   core,
		gw:     gw,
		stopCh: make(chan struct{}),
	}
}

// Start dials the broker, declares topology idempotently, enables publisher
// confirms, and starts the consumer goroutine.
func (q *Queue) Start(_ context.Context) error {
	conn, err := amqp.Dial(q.url)
	if err != nil {
		return fmt.Errorf("rabbitmq: dial %q: %w", q.url, err)
	}
	q.conn = conn

	pubCh, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("rabbitmq: open publisher channel: %w", err)
	}
	q.pubCh = pubCh

	consCh, err := conn.Channel()
	if err != nil {
		_ = pubCh.Close()
		_ = conn.Close()
		return fmt.Errorf("rabbitmq: open consumer channel: %w", err)
	}
	q.consCh = consCh

	if err := declareTopology(pubCh); err != nil {
		_ = consCh.Close()
		_ = pubCh.Close()
		_ = conn.Close()
		return err
	}

	if err := pubCh.Confirm(false); err != nil {
		_ = consCh.Close()
		_ = pubCh.Close()
		_ = conn.Close()
		return fmt.Errorf("rabbitmq: enable publisher confirms: %w", err)
	}
	q.confirms = pubCh.NotifyPublish(make(chan amqp.Confirmation, 128))

	deliveries, err := consCh.Consume(mainQueue, "", false, false, false, false, nil)
	if err != nil {
		_ = consCh.Close()
		_ = pubCh.Close()
		_ = conn.Close()
		return fmt.Errorf("rabbitmq: start consumer: %w", err)
	}

	q.wg.Add(1)
	go q.runConsumer(deliveries)
	return nil
}

// Enqueue implements [ingest.Queue].
// It marshals the task, publishes it to the main exchange with publisher
// confirms enabled, and blocks until the broker acknowledges the message or
// the context is cancelled.
func (q *Queue) Enqueue(ctx context.Context, task ingest.Task) error {
	select {
	case <-q.stopCh:
		return brainapi.E(brainapi.KindInternal, "rabbitmq_enqueue", "queue is stopped", nil)
	default:
	}

	body, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("rabbitmq: marshal task: %w", err)
	}
	return q.publish(ctx, body, 0)
}

// Stop signals the consumer goroutine to exit, waits for it to drain, then
// closes the AMQP channels and connection.  It is safe to call multiple times.
func (q *Queue) Stop() {
	q.once.Do(func() { close(q.stopCh) })
	q.wg.Wait()
	if q.consCh != nil {
		_ = q.consCh.Cancel("", false)
		_ = q.consCh.Close()
	}
	if q.pubCh != nil {
		_ = q.pubCh.Close()
	}
	if q.conn != nil {
		_ = q.conn.Close()
	}
}

// publish serialises concurrent callers so that each publish + confirm pair
// stays atomic.  The mutex is held for the duration of the confirm wait.
func (q *Queue) publish(ctx context.Context, body []byte, retryCount int32) error {
	q.pubMu.Lock()
	defer q.pubMu.Unlock()

	err := q.pubCh.PublishWithContext(ctx, mainExchange, mainRouteKey, false, false,
		amqp.Publishing{
			ContentType:  "application/json",
			Body:         body,
			DeliveryMode: amqp.Persistent,
			Headers:      amqp.Table{headerRetry: retryCount},
		},
	)
	if err != nil {
		return fmt.Errorf("rabbitmq: publish: %w", err)
	}

	select {
	case confirm, ok := <-q.confirms:
		if !ok {
			return brainapi.E(brainapi.KindInternal, "rabbitmq_enqueue", "confirm channel closed", nil)
		}
		if !confirm.Ack {
			return brainapi.E(brainapi.KindInternal, "rabbitmq_enqueue", "broker nacked message", nil)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-q.stopCh:
		return brainapi.E(brainapi.KindInternal, "rabbitmq_enqueue", "queue stopped while awaiting confirm", nil)
	}
}

// runConsumer dispatches deliveries to handleDelivery until the stop signal or
// the delivery channel is closed by the broker.
func (q *Queue) runConsumer(deliveries <-chan amqp.Delivery) {
	defer q.wg.Done()
	for {
		select {
		case d, ok := <-deliveries:
			if !ok {
				return
			}
			q.handleDelivery(d)
		case <-q.stopCh:
			return
		}
	}
}

// handleDelivery unmarshals a delivery and either processes it successfully or
// delegates error handling to retryOrDLQ.
func (q *Queue) handleDelivery(d amqp.Delivery) {
	var task ingest.Task
	if err := json.Unmarshal(d.Body, &task); err != nil {
		slog.Error("rabbitmq: unmarshal failed — routing to DLQ", "err", err)
		_ = d.Nack(false, false) // no requeue → DLX → DLQ
		return
	}
	if err := q.processTask(task); err != nil {
		q.retryOrDLQ(d, err)
		return
	}
	_ = d.Ack(false)
}

// processTask drives both completers to mark the job terminal.
// The core completer is best-effort; only the gateway completer error is returned
// so it can be retried.
func (q *Queue) processTask(task ingest.Task) error {
	ctx := context.Background()
	_ = q.core.CompleteJob(task.TenantID, task.JobID, brainapi.JobCompleted, "", "")
	_, err := q.gw.HandleJobCompleted(ctx, task.TenantID, task.JobID, brainapi.JobCompleted, "", "")
	return err
}

// retryOrDLQ republishes the message with an incremented retry counter after
// exponential back-off, or nacks it to the DLQ when retries are exhausted.
// Back-off delays: 500ms, 1s, 2s (for retry counts 0, 1, 2).
func (q *Queue) retryOrDLQ(d amqp.Delivery, processErr error) {
	count := extractRetryCount(d.Headers)
	if count >= maxRetries {
		slog.Error("rabbitmq: max retries exceeded — routing to DLQ",
			"retries", count, "err", processErr)
		_ = d.Nack(false, false) // no requeue → DLX → DLQ
		return
	}

	delay := retryBaseWait * (1 << count) // 500ms, 1s, 2s
	select {
	case <-time.After(delay):
	case <-q.stopCh:
		_ = d.Nack(false, false)
		return
	}

	if err := q.publish(context.Background(), d.Body, count+1); err != nil {
		slog.Error("rabbitmq: republish for retry failed — routing to DLQ", "err", err)
		_ = d.Nack(false, false)
		return
	}
	_ = d.Ack(false) // ack original; retry is the republished copy
}

// extractRetryCount reads the x-wb-retry-count header value, defaulting to 0.
func extractRetryCount(headers amqp.Table) int32 {
	if headers == nil {
		return 0
	}
	v, ok := headers[headerRetry]
	if !ok {
		return 0
	}
	count, _ := v.(int32)
	return count
}

// declareTopology declares exchanges and queues idempotently (durable, non-exclusive).
// Order matters: DLX and DLQ must exist before the main queue references the DLX.
func declareTopology(ch *amqp.Channel) error {
	// Dead-letter exchange (receives nacked/TTL-expired messages from main queue).
	if err := ch.ExchangeDeclare(dlxExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: declare DLX: %w", err)
	}
	// Dead-letter queue — inspect here to debug poison messages.
	if _, err := ch.QueueDeclare(dlqQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: declare DLQ: %w", err)
	}
	if err := ch.QueueBind(dlqQueue, dlqRouteKey, dlxExchange, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: bind DLQ to DLX: %w", err)
	}

	// Main ingest exchange.
	if err := ch.ExchangeDeclare(mainExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: declare main exchange: %w", err)
	}
	// Main task queue wired to the DLX so nacked messages land in the DLQ.
	args := amqp.Table{
		"x-dead-letter-exchange":    dlxExchange,
		"x-dead-letter-routing-key": dlqRouteKey,
	}
	if _, err := ch.QueueDeclare(mainQueue, true, false, false, false, args); err != nil {
		return fmt.Errorf("rabbitmq: declare main queue: %w", err)
	}
	if err := ch.QueueBind(mainQueue, mainRouteKey, mainExchange, false, nil); err != nil {
		return fmt.Errorf("rabbitmq: bind main queue: %w", err)
	}
	return nil
}
