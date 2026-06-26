// Package memory provides an in-memory data core for tests and local demos.
package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

const embeddingDimensions = 16

// Core is a thread-safe in-memory implementation of brainapi.Core.
type Core struct {
	mu              sync.RWMutex
	now             func() time.Time
	persistencePath string
	initErr         error
	bindings        map[brainapi.BindingKey]brainapi.TenantID
	projects        map[brainapi.TenantID]project
	jobs            map[string]brainapi.JobSnapshot
	sources         map[brainapi.TenantID][]brainapi.SourceRef
	docs            map[brainapi.TenantID][]document
	chunks          map[brainapi.TenantID][]chunk
}

type project struct {
	owner    brainapi.Principal
	metadata map[string]string
	state    brainapi.ProjectState
}

type document struct {
	source  brainapi.SourceRef
	title   string
	content string
	freshAt time.Time
}

type chunk struct {
	id      string
	source  brainapi.Source
	text    string
	terms   map[string]int
	vector  []float64
	freshAt time.Time
	kind    string
}

type scoredChunk struct {
	chunk chunk
	score float64
	order int
}

// Option configures Core.
type Option func(*Core)

// WithClock injects a deterministic clock.
func WithClock(clock func() time.Time) Option {
	return func(c *Core) {
		if clock != nil {
			c.now = clock
		}
	}
}

// WithPersistence enables durable JSON snapshot persistence at path.
func WithPersistence(path string) Option {
	return func(c *Core) {
		c.persistencePath = strings.TrimSpace(path)
	}
}

// New creates an empty in-memory data core.
func New(opts ...Option) *Core {
	c := newCore(opts...)
	if c.persistencePath != "" {
		c.initErr = c.loadSnapshot()
	}
	return c
}

// NewPersistent creates a memory core backed by an atomic JSON snapshot file.
func NewPersistent(path string, opts ...Option) (*Core, error) {
	opts = append([]Option{WithPersistence(path)}, opts...)
	c := newCore(opts...)
	if err := c.loadSnapshot(); err != nil {
		return nil, err
	}
	return c, nil
}

func newCore(opts ...Option) *Core {
	c := &Core{
		now:      time.Now,
		bindings: make(map[brainapi.BindingKey]brainapi.TenantID),
		projects: make(map[brainapi.TenantID]project),
		jobs:     make(map[string]brainapi.JobSnapshot),
		sources:  make(map[brainapi.TenantID][]brainapi.SourceRef),
		docs:     make(map[brainapi.TenantID][]document),
		chunks:   make(map[brainapi.TenantID][]chunk),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ResolveBinding returns tenant_id for a surface-neutral binding key.
func (c *Core) ResolveBinding(ctx context.Context, binding brainapi.BindingKey) (brainapi.TenantID, error) {
	_ = ctx
	if err := brainapi.ValidateBindingKey(binding); err != nil {
		return "", err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("resolve_binding"); err != nil {
		return "", err
	}
	tenantID, ok := c.bindings[binding]
	if !ok {
		return "", brainapi.E(brainapi.KindNotFound, "resolve_binding", "binding not found", nil)
	}
	return tenantID, nil
}

// CreateProject atomically provisions a tenant and binding.
func (c *Core) CreateProject(ctx context.Context, req brainapi.CreateProjectRequest) error {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateBindingKey(req.BindingKey); err != nil {
		return err
	}
	if req.OwnerPrincipal.ID == "" {
		return brainapi.E(brainapi.KindInvalid, "create_project", "owner principal is required", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireReadyLocked("create_project"); err != nil {
		return err
	}
	if _, ok := c.projects[req.TenantID]; ok {
		return brainapi.E(brainapi.KindAlreadyExists, "create_project", "tenant already exists", nil)
	}
	if _, ok := c.bindings[req.BindingKey]; ok {
		return brainapi.E(brainapi.KindAlreadyExists, "create_project", "binding already exists", nil)
	}
	c.projects[req.TenantID] = project{owner: req.OwnerPrincipal, metadata: cloneMap(req.Metadata), state: brainapi.ProjectOn}
	c.bindings[req.BindingKey] = req.TenantID
	return c.persistLocked()
}

// Ingest stores source metadata, creates deterministic chunks, and records a running job.
func (c *Core) Ingest(ctx context.Context, req brainapi.IngestRequest) error {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateJobID(req.JobID); err != nil {
		return err
	}
	if strings.TrimSpace(req.Source.URI) == "" {
		return brainapi.E(brainapi.KindInvalid, "ingest", "source uri is required", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireReadyLocked("ingest"); err != nil {
		return err
	}
	if err := c.requireProjectLocked(req.TenantID, "ingest"); err != nil {
		return err
	}
	key := jobKey(req.TenantID, req.JobID)
	if _, ok := c.jobs[key]; ok {
		return brainapi.E(brainapi.KindAlreadyExists, "ingest", "job already exists", nil)
	}
	doc := documentFrom(req, c.now().UTC())
	sourceIndex := len(c.sources[req.TenantID])
	newChunks := chunksFrom(req.TenantID, sourceIndex, doc, len(c.chunks[req.TenantID]))
	c.sources[req.TenantID] = append(c.sources[req.TenantID], req.Source)
	c.docs[req.TenantID] = append(c.docs[req.TenantID], doc)
	c.chunks[req.TenantID] = append(c.chunks[req.TenantID], newChunks...)
	c.jobs[key] = brainapi.JobSnapshot{TenantID: req.TenantID, JobID: req.JobID, Status: brainapi.JobRunning, UpdatedAt: c.now().UTC()}
	return c.persistLocked()
}

// CompleteJob marks a job terminal. It simulates worker callback state for tests and demos.
func (c *Core) CompleteJob(tenantID brainapi.TenantID, jobID brainapi.JobID, status brainapi.JobStatus, resultRef, message string) error {
	if !status.Final() {
		return brainapi.E(brainapi.KindInvalid, "complete_job", "status must be terminal", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireReadyLocked("complete_job"); err != nil {
		return err
	}
	key := jobKey(tenantID, jobID)
	snapshot, ok := c.jobs[key]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, "complete_job", "job not found", nil)
	}
	snapshot.Status = status
	snapshot.ResultRef = resultRef
	snapshot.Error = message
	snapshot.UpdatedAt = c.now().UTC()
	c.jobs[key] = snapshot
	return c.persistLocked()
}

// JobStatus returns tenant-scoped job state.
func (c *Core) JobStatus(ctx context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error) {
	_ = ctx
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	if err := brainapi.ValidateJobID(jobID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("job_status"); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	snapshot, ok := c.jobs[jobKey(tenantID, jobID)]
	if !ok {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindNotFound, "job_status", "job not found", nil)
	}
	return snapshot, nil
}

// Query retrieves tenant-scoped chunks and returns a deterministic grounded answer.
func (c *Core) Query(ctx context.Context, req brainapi.QueryRequest) (brainapi.QueryResponse, error) {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return brainapi.QueryResponse{}, err
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		return brainapi.QueryResponse{}, brainapi.E(brainapi.KindInvalid, "query", "question is required", nil)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("query"); err != nil {
		return brainapi.QueryResponse{}, err
	}
	if err := c.requireProjectLocked(req.TenantID, "query"); err != nil {
		return brainapi.QueryResponse{}, err
	}
	ranked := rankChunks(question, c.chunks[req.TenantID])
	if len(ranked) == 0 {
		return brainapi.QueryResponse{Answer: "수집된 근거가 없습니다: " + question, GroundingAvailable: false}, nil
	}
	top := topChunks(ranked, 3)
	return brainapi.QueryResponse{
		Answer:             synthesize(question, top),
		Sources:            uniqueSources(top),
		GroundedSpans:      groundedSpans(top),
		SupplementedSpans:  nil,
		GroundingAvailable: true,
	}, nil
}

// Discover returns tenant-scoped metadata only.
func (c *Core) Discover(ctx context.Context, req brainapi.DiscoverRequest) (brainapi.DiscoverResponse, error) {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return brainapi.DiscoverResponse{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("discover"); err != nil {
		return brainapi.DiscoverResponse{}, err
	}
	if err := c.requireProjectLocked(req.TenantID, "discover"); err != nil {
		return brainapi.DiscoverResponse{}, err
	}
	queryTerms := terms(req.Query)
	results := make([]brainapi.MetadataResult, 0, len(c.docs[req.TenantID]))
	for i, doc := range c.docs[req.TenantID] {
		if len(queryTerms) > 0 && overlap(queryTerms, terms(doc.title+" "+doc.source.URI+" "+doc.content)) == 0 {
			continue
		}
		results = append(results, brainapi.MetadataResult{ID: string(req.TenantID) + ":metadata:" + string(rune('a'+i%26)), Title: doc.title, Kind: doc.source.MimeType, FreshAt: doc.freshAt})
	}
	return brainapi.DiscoverResponse{Results: results}, nil
}

// SetProjectState freezes or enables a tenant.
func (c *Core) SetProjectState(ctx context.Context, tenantID brainapi.TenantID, state brainapi.ProjectState) error {
	_ = ctx
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return err
	}
	if state != brainapi.ProjectOn && state != brainapi.ProjectOff {
		return brainapi.E(brainapi.KindInvalid, "set_project_state", "unknown project state", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireReadyLocked("set_project_state"); err != nil {
		return err
	}
	proj, ok := c.projects[tenantID]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, "set_project_state", "tenant not found", nil)
	}
	proj.state = state
	c.projects[tenantID] = proj
	return c.persistLocked()
}

func (c *Core) requireReadyLocked(op string) error {
	if c.initErr != nil {
		return brainapi.E(brainapi.KindInternal, op, "memory core persistence is unavailable", c.initErr)
	}
	return nil
}

func (c *Core) requireProjectLocked(tenantID brainapi.TenantID, op string) error {
	proj, ok := c.projects[tenantID]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, op, "tenant not found", nil)
	}
	if proj.state == brainapi.ProjectOff {
		return brainapi.E(brainapi.KindConflict, op, "project is off", nil)
	}
	return nil
}

func documentFrom(req brainapi.IngestRequest, freshAt time.Time) document {
	title := req.Source.Name
	if title == "" {
		title = req.Source.URI
	}
	content := strings.TrimSpace(req.Metadata["content"])
	if content == "" {
		content = strings.TrimSpace(title + " " + req.Source.URI + " " + req.Source.MimeType)
	}
	return document{source: req.Source, title: title, content: content, freshAt: freshAt}
}

func chunksFrom(tenantID brainapi.TenantID, sourceIndex int, doc document, offset int) []chunk {
	words := strings.Fields(doc.content)
	if len(words) == 0 {
		words = []string{doc.title}
	}
	const chunkWords = 48
	chunks := make([]chunk, 0, (len(words)+chunkWords-1)/chunkWords)
	for start := 0; start < len(words); start += chunkWords {
		end := start + chunkWords
		if end > len(words) {
			end = len(words)
		}
		text := strings.Join(words[start:end], " ")
		id := string(tenantID) + ":chunk:" + string(rune('a'+(offset+len(chunks))%26))
		chunks = append(chunks, chunk{
			id: id,
			source: brainapi.Source{
				ID:       string(tenantID) + ":source:" + string(rune('a'+sourceIndex%26)),
				Title:    doc.title,
				URI:      doc.source.URI,
				TenantID: tenantID,
			},
			text:    text,
			terms:   terms(text),
			vector:  embed(text),
			freshAt: doc.freshAt,
			kind:    doc.source.MimeType,
		})
	}
	return chunks
}

func rankChunks(question string, chunks []chunk) []scoredChunk {
	questionTerms := terms(question)
	questionVector := embed(question)
	ranked := make([]scoredChunk, 0, len(chunks))
	for i, ch := range chunks {
		score := float64(overlap(questionTerms, ch.terms))*2 + cosine(questionVector, ch.vector)
		ranked = append(ranked, scoredChunk{chunk: ch, score: score, order: i})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score == ranked[j].score {
			return ranked[i].order < ranked[j].order
		}
		return ranked[i].score > ranked[j].score
	})
	return ranked
}

func topChunks(ranked []scoredChunk, limit int) []chunk {
	if len(ranked) < limit {
		limit = len(ranked)
	}
	out := make([]chunk, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, ranked[i].chunk)
	}
	return out
}

func synthesize(question string, chunks []chunk) string {
	if len(chunks) == 0 {
		return "수집된 근거가 없습니다: " + question
	}
	return "근거 기반 응답: " + chunks[0].text
}

func uniqueSources(chunks []chunk) []brainapi.Source {
	seen := make(map[string]bool)
	sources := make([]brainapi.Source, 0, len(chunks))
	for _, ch := range chunks {
		key := ch.source.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		sources = append(sources, ch.source)
	}
	return sources
}

func groundedSpans(chunks []chunk) []string {
	spans := make([]string, 0, len(chunks))
	for _, ch := range chunks {
		spans = append(spans, ch.text)
	}
	return spans
}

func terms(text string) map[string]int {
	out := make(map[string]int)
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if token != "" {
			out[token]++
		}
	}
	return out
}

func overlap(left, right map[string]int) int {
	count := 0
	for token, leftCount := range left {
		if rightCount := right[token]; rightCount > 0 {
			if leftCount < rightCount {
				count += leftCount
			} else {
				count += rightCount
			}
		}
	}
	return count
}

func embed(text string) []float64 {
	vector := make([]float64, embeddingDimensions)
	for token, count := range terms(text) {
		idx := stableHash(token) % embeddingDimensions
		vector[idx] += float64(count)
	}
	return vector
}

func cosine(left, right []float64) float64 {
	var dot, leftNorm, rightNorm float64
	for i := range left {
		dot += left[i] * right[i]
		leftNorm += left[i] * left[i]
		rightNorm += right[i] * right[i]
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

func stableHash(token string) int {
	var h uint32 = 2166136261
	for _, r := range token {
		h ^= uint32(r)
		h *= 16777619
	}
	return int(h)
}

func cloneMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func jobKey(tenantID brainapi.TenantID, jobID brainapi.JobID) string {
	return string(tenantID) + "\x00" + string(jobID)
}
