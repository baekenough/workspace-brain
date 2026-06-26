// Package qdrant provides a tenant-scoped Qdrant VectorStore adapter.
//
// # Tenant isolation guarantee
//
// Each tenant receives its own Qdrant collection named "wb_<sanitized_tenantID>".
// Because Add, Search, Len, TruncateTo, and Replace all target the per-tenant
// collection exclusively, it is structurally impossible for a search on
// tenant A to return vectors belonging to tenant B — no filter is required.
//
// # Interface contract
//
// Store implements memory.VectorStore. The interface methods do not return
// errors; transient failures are retried up to maxRetries times with
// exponential back-off. Permanent failures (e.g. network partition) are
// silently swallowed: Search returns nil, Len returns 0. For observability,
// the caller may poll Store.Err() to retrieve the most recent non-retryable
// error after a sequence of operations.
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sangyi/workspace-brain/internal/core/memory"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

const (
	defaultTimeout = 10 * time.Second
	maxRetries     = 3
	retryBase      = 50 * time.Millisecond
)

// Store is a memory.VectorStore backed by Qdrant.
// Create with New; zero value is not usable.
type Store struct {
	baseURL   string
	vectorDim int
	client    *http.Client

	mu      sync.Mutex
	lastErr error
}

// Compile-time check: Store must implement memory.VectorStore.
var _ memory.VectorStore = (*Store)(nil)

// New returns a Store that talks to the Qdrant instance at addr
// (e.g. "http://localhost:6333"). vectorDim must equal the number of
// dimensions produced by the memory.Embedder wired into memory.Core —
// the in-memory default (localEmbedder) uses 16 dimensions.
func New(addr string, vectorDim int) *Store {
	return &Store{
		baseURL:   strings.TrimRight(addr, "/"),
		vectorDim: vectorDim,
		client:    &http.Client{Timeout: defaultTimeout},
	}
}

// Err returns the most recent non-retryable error encountered by any
// VectorStore method, or nil if all operations have succeeded.
// Calling Err resets the stored error.
func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.lastErr
	s.lastErr = nil
	return err
}

// ─── VectorStore implementation ───────────────────────────────────────────────

// Add ensures the tenant's collection exists, then upserts all chunks.
// Idempotent: re-upserting a chunk with the same ID overwrites it with
// identical data.
func (s *Store) Add(tenantID brainapi.TenantID, chunks []memory.Chunk) {
	if len(chunks) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	if err := s.ensureCollection(ctx, tenantID); err != nil {
		s.storeErr(err)
		return
	}
	if err := s.upsert(ctx, tenantID, chunks); err != nil {
		s.storeErr(err)
	}
}

// Search queries the tenant's Qdrant collection with the provided embedding
// vector and returns up to limit chunks, ordered by descending cosine
// similarity. questionTerms is accepted for interface compatibility but is not
// used for Qdrant-side filtering; the local Reranker re-scores candidates
// using terms after Search returns.
func (s *Store) Search(tenantID brainapi.TenantID, _ map[string]int, questionVector []float64, limit int) []memory.Chunk {
	if limit <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	coll := collectionName(tenantID)
	body := map[string]any{
		"vector":       questionVector,
		"limit":        limit,
		"with_payload": true,
	}
	status, data, err := s.doWithRetry(ctx, http.MethodPost, "/collections/"+coll+"/points/search", body)
	if err != nil {
		s.storeErr(err)
		return nil
	}
	// 404 means the collection does not exist yet — no chunks ingested.
	if status == http.StatusNotFound {
		return nil
	}
	if status != http.StatusOK {
		s.storeErr(fmt.Errorf("qdrant: search %s: status %d: %s", coll, status, data))
		return nil
	}

	var resp struct {
		Result []struct {
			ID      uint64          `json:"id"`
			Score   float64         `json:"score"`
			Payload json.RawMessage `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		s.storeErr(fmt.Errorf("qdrant: decode search response: %w", err))
		return nil
	}

	chunks := make([]memory.Chunk, 0, len(resp.Result))
	for _, r := range resp.Result {
		ch, ok := payloadToChunk(r.Payload)
		if !ok {
			continue
		}
		chunks = append(chunks, ch)
	}
	return chunks
}

// Len returns the number of points in the tenant's collection.
// Returns 0 if the collection does not yet exist.
func (s *Store) Len(tenantID brainapi.TenantID) int {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	coll := collectionName(tenantID)
	status, data, err := s.doWithRetry(ctx, http.MethodPost, "/collections/"+coll+"/points/count", map[string]any{})
	if err != nil {
		s.storeErr(err)
		return 0
	}
	if status == http.StatusNotFound {
		return 0
	}
	if status != http.StatusOK {
		s.storeErr(fmt.Errorf("qdrant: count %s: status %d", coll, status))
		return 0
	}

	var resp struct {
		Result struct {
			Count int `json:"count"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		s.storeErr(fmt.Errorf("qdrant: decode count response: %w", err))
		return 0
	}
	return resp.Result.Count
}

// TruncateTo deletes all points whose stored position is >= n.
// This is the rollback path used by memory.Core when persistence fails after
// Add: position == chunk index in the per-tenant sequence (embedded in the
// chunk ID as the last colon-separated component).
func (s *Store) TruncateTo(tenantID brainapi.TenantID, n int) {
	if n < 0 {
		n = 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	coll := collectionName(tenantID)
	// Delete all points with payload.position >= n.
	body := map[string]any{
		"filter": map[string]any{
			"must": []map[string]any{
				{
					"key": "position",
					"range": map[string]any{
						"gte": n,
					},
				},
			},
		},
	}
	status, data, err := s.doWithRetry(ctx, http.MethodPost,
		"/collections/"+coll+"/points/delete?wait=true", body)
	if err != nil {
		s.storeErr(err)
		return
	}
	if status == http.StatusNotFound {
		return // collection doesn't exist — nothing to truncate
	}
	if status != http.StatusOK {
		s.storeErr(fmt.Errorf("qdrant: truncate %s to %d: status %d: %s", coll, n, status, data))
	}
}

// Replace deletes the tenant's collection and recreates it with the provided
// chunks. This is the snapshot-reload path: a clean slate guarantees the
// store contains exactly the persisted chunks and no stale data.
func (s *Store) Replace(tenantID brainapi.TenantID, chunks []memory.Chunk) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	coll := collectionName(tenantID)
	// Delete the collection (ignore 404 — it may not exist yet).
	status, _, err := s.doWithRetry(ctx, http.MethodDelete, "/collections/"+coll, nil)
	if err != nil {
		s.storeErr(err)
		return
	}
	if status != http.StatusOK && status != http.StatusNotFound {
		s.storeErr(fmt.Errorf("qdrant: delete collection %s: status %d", coll, status))
		return
	}

	if len(chunks) == 0 {
		return // nothing to insert after deleting
	}

	if err := s.ensureCollection(ctx, tenantID); err != nil {
		s.storeErr(err)
		return
	}
	if err := s.upsert(ctx, tenantID, chunks); err != nil {
		s.storeErr(err)
	}
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// collectionName returns a Qdrant-safe collection name for the tenant.
// Non-alphanumeric characters are replaced with underscores; "wb_" prefix
// distinguishes workspace-brain collections from Qdrant system names.
func collectionName(tenantID brainapi.TenantID) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, string(tenantID))
	return "wb_" + safe
}

// ensureCollection creates the per-tenant Qdrant collection if it does not
// already exist. Idempotent: a second call when the collection exists is a
// cheap GET + early return.
func (s *Store) ensureCollection(ctx context.Context, tenantID brainapi.TenantID) error {
	coll := collectionName(tenantID)

	// Fast-path: check existence.
	status, _, err := s.doWithRetry(ctx, http.MethodGet, "/collections/"+coll, nil)
	if err != nil {
		return fmt.Errorf("qdrant: check collection %s: %w", coll, err)
	}
	if status == http.StatusOK {
		return nil
	}

	// Create with cosine distance; dimension must match the configured Embedder.
	body := map[string]any{
		"vectors": map[string]any{
			"size":     s.vectorDim,
			"distance": "Cosine",
		},
	}
	status, data, err := s.doWithRetry(ctx, http.MethodPut, "/collections/"+coll, body)
	if err != nil {
		return fmt.Errorf("qdrant: create collection %s: %w", coll, err)
	}
	if status == http.StatusOK {
		return nil
	}
	return fmt.Errorf("qdrant: create collection %s: status %d: %s", coll, status, data)
}

// pointPayload is the JSON payload stored for every Qdrant point.
// It carries enough information to reconstruct a memory.Chunk on retrieval.
type pointPayload struct {
	ChunkID     string         `json:"chunk_id"`
	Position    int            `json:"position"`
	Text        string         `json:"text"`
	SourceID    string         `json:"source_id"`
	SourceTitle string         `json:"source_title"`
	SourceURI   string         `json:"source_uri"`
	TenantID    string         `json:"tenant_id"`
	FreshAtUnix int64          `json:"fresh_at_unix"`
	Kind        string         `json:"kind"`
	Terms       map[string]int `json:"terms,omitempty"`
}

// positionFromID extracts the numeric position from a chunk ID of the form
// "{tenantID}:chunk:{N}". Returns (0, false) on malformed input.
func positionFromID(id string) (int, bool) {
	idx := strings.LastIndex(id, ":")
	if idx < 0 || idx == len(id)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(id[idx+1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// upsert sends all chunks to Qdrant in a single PUT /points?wait=true request.
// The ?wait=true parameter makes the call synchronous: the HTTP response is
// not returned until the upsert is durable, providing idempotent behaviour on
// retry (re-upserting the same point ID is a no-op if the payload is identical).
func (s *Store) upsert(ctx context.Context, tenantID brainapi.TenantID, chunks []memory.Chunk) error {
	coll := collectionName(tenantID)
	type point struct {
		ID      uint64        `json:"id"`
		Vector  []float64     `json:"vector"`
		Payload *pointPayload `json:"payload"`
	}
	points := make([]point, 0, len(chunks))
	for _, ch := range chunks {
		pos, ok := positionFromID(ch.ID)
		if !ok {
			// Fallback: use slice index within this batch (shouldn't happen with
			// chunks produced by memory.chunksFrom).
			pos = len(points)
		}
		points = append(points, point{
			ID:     uint64(pos),
			Vector: ch.Vector,
			Payload: &pointPayload{
				ChunkID:     ch.ID,
				Position:    pos,
				Text:        ch.Text,
				SourceID:    ch.Source.ID,
				SourceTitle: ch.Source.Title,
				SourceURI:   ch.Source.URI,
				TenantID:    string(ch.Source.TenantID),
				FreshAtUnix: ch.FreshAt.Unix(),
				Kind:        ch.Kind,
				Terms:       ch.Terms,
			},
		})
	}

	body := map[string]any{"points": points}
	status, data, err := s.doWithRetry(ctx, http.MethodPut,
		"/collections/"+coll+"/points?wait=true", body)
	if err != nil {
		return fmt.Errorf("qdrant: upsert to %s: %w", coll, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("qdrant: upsert to %s: status %d: %s", coll, status, data)
	}
	return nil
}

// payloadToChunk reconstructs a memory.Chunk from a Qdrant point payload.
func payloadToChunk(raw json.RawMessage) (memory.Chunk, bool) {
	var p pointPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return memory.Chunk{}, false
	}
	return memory.Chunk{
		ID: p.ChunkID,
		Source: brainapi.Source{
			ID:       p.SourceID,
			Title:    p.SourceTitle,
			URI:      p.SourceURI,
			TenantID: brainapi.TenantID(p.TenantID),
		},
		Text:    p.Text,
		Terms:   p.Terms,
		Vector:  nil, // vectors are not returned by search by default; reranker recomputes from text
		FreshAt: time.Unix(p.FreshAtUnix, 0).UTC(),
		Kind:    p.Kind,
	}, true
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

// doWithRetry executes an HTTP request, retrying on 5xx or network errors
// up to maxRetries times with exponential back-off (50 ms, 100 ms, 200 ms).
// 4xx responses are not retried (they indicate a client-side problem).
func (s *Store) doWithRetry(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var lastErr error
	delay := retryBase
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			case <-time.After(delay):
				delay *= 2
			}
		}
		status, data, err := s.doOnce(ctx, method, path, body)
		if err != nil {
			lastErr = err
			continue // network error — retry
		}
		if status >= 500 {
			lastErr = fmt.Errorf("qdrant: %s %s: server error %d: %s", method, path, status, data)
			continue // server error — retry
		}
		return status, data, nil
	}
	return 0, nil, lastErr
}

// doOnce performs a single HTTP request and returns the status code and body.
func (s *Store) doOnce(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("qdrant: marshal request body: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, r)
	if err != nil {
		return 0, nil, fmt.Errorf("qdrant: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("qdrant: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("qdrant: read response body: %w", err)
	}
	return resp.StatusCode, data, nil
}

func (s *Store) storeErr(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
}
