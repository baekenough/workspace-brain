// Package memory provides an in-memory data core for tests and local demos.
package memory

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

const embeddingDimensions = 16

// ─── Consumer-side seam interfaces ──────────────────────────────────────────

// Embedder converts a text string into a fixed-length embedding vector.
// Implementations must be deterministic for the same input.
// Memory never imports internal/core/ai; this is the consumer-side declaration.
type Embedder interface {
	Embed(text string) []float64
}

// VectorStore stores pre-embedded chunks and performs ranked similarity search.
// All methods are tenant-scoped to preserve isolation guarantees.
// VectorStore methods are called while Core.mu is held; the in-memory default
// therefore requires no additional synchronisation of its own.
type VectorStore interface {
	// Add appends chunks for a tenant (called during Ingest).
	Add(tenantID brainapi.TenantID, chunks []chunk)
	// Search returns the top-limit ranked chunks matching the pre-embedded query.
	Search(tenantID brainapi.TenantID, questionTerms map[string]int, questionVector []float64, limit int) []chunk
	// Len returns the current chunk count for a tenant (used before Ingest for rollback).
	Len(tenantID brainapi.TenantID) int
	// TruncateTo removes chunks beyond n for a tenant (Ingest rollback on persist failure).
	TruncateTo(tenantID brainapi.TenantID, n int)
	// Replace overwrites all chunks for a tenant (used during snapshot load).
	Replace(tenantID brainapi.TenantID, chunks []chunk)
}

// SynthesisInput is a single retrieved passage passed to a Synthesizer.
type SynthesisInput struct {
	Text string
}

// SynthesisResult is the output of a Synthesizer: the answer text and a
// breakdown of content that came directly from retrieved passages (grounded)
// versus content the model generated beyond those passages (supplemented).
type SynthesisResult struct {
	Answer            string
	GroundedSpans     []string
	SupplementedSpans []string
}

// Synthesizer generates a grounded answer from retrieved passages.
// Implementations must handle an empty inputs slice (return abstention).
// Memory never imports internal/core/ai; adapters are bridged in cmd/main.go.
type Synthesizer interface {
	Synthesize(ctx context.Context, question string, inputs []SynthesisInput) (SynthesisResult, error)
}

// Reranker orders candidate chunk texts for a question, returning their
// original indices (into the candidateTexts slice) in descending relevance order.
// The default localReranker applies the overlap×2 + cosine formula.
type Reranker interface {
	Rerank(question string, candidateTexts []string) []int
}

// ─── Default (local) implementations ────────────────────────────────────────

// localEmbedder is the default Embedder. It uses the deterministic FNV-1a
// bag-of-terms hashing that the core used before the seam was introduced, so
// all vectors and rankings remain identical to the original behaviour.
type localEmbedder struct{}

func (localEmbedder) Embed(text string) []float64 {
	return embed(text)
}

// inMemoryVectorStore is the default VectorStore. It reproduces the original
// ranking formula exactly: score = overlap(terms)*2 + cosine(vectors), top-N.
type inMemoryVectorStore struct {
	chunks map[brainapi.TenantID][]chunk
}

func newInMemoryVectorStore() *inMemoryVectorStore {
	return &inMemoryVectorStore{chunks: make(map[brainapi.TenantID][]chunk)}
}

func (s *inMemoryVectorStore) Add(tenantID brainapi.TenantID, chunks []chunk) {
	s.chunks[tenantID] = append(s.chunks[tenantID], chunks...)
}

func (s *inMemoryVectorStore) Search(tenantID brainapi.TenantID, questionTerms map[string]int, questionVector []float64, limit int) []chunk {
	return topChunks(rankChunks(questionTerms, questionVector, s.chunks[tenantID]), limit)
}

func (s *inMemoryVectorStore) Len(tenantID brainapi.TenantID) int {
	return len(s.chunks[tenantID])
}

func (s *inMemoryVectorStore) TruncateTo(tenantID brainapi.TenantID, n int) {
	if n < len(s.chunks[tenantID]) {
		s.chunks[tenantID] = s.chunks[tenantID][:n]
	}
}

func (s *inMemoryVectorStore) Replace(tenantID brainapi.TenantID, chunks []chunk) {
	dst := make([]chunk, len(chunks))
	copy(dst, chunks)
	s.chunks[tenantID] = dst
}

// localSynthesizer is the default Synthesizer. It produces a deterministic
// answer from the top retrieved passage without any external API calls.
// All retrieved passages are reported as grounded; SupplementedSpans is nil
// because no model-generated content is added beyond the direct citation.
type localSynthesizer struct{}

func (localSynthesizer) Synthesize(_ context.Context, question string, inputs []SynthesisInput) (SynthesisResult, error) {
	if len(inputs) == 0 {
		return SynthesisResult{Answer: "수집된 근거가 없습니다: " + question}, nil
	}
	spans := make([]string, len(inputs))
	for i, inp := range inputs {
		spans[i] = inp.Text
	}
	return SynthesisResult{
		Answer:        "근거 기반 응답: " + inputs[0].Text,
		GroundedSpans: spans,
		// SupplementedSpans is nil: the answer is a direct citation, not model-supplemented.
	}, nil
}

// localReranker is the default Reranker. It applies the overlap×2 + cosine
// formula – the same scoring used by inMemoryVectorStore.Search – so the
// retrieval order from VectorStore is preserved when the Reranker is not
// replaced. Injecting a cross-encoder implementation upgrades quality here
// without touching any other part of the pipeline.
type localReranker struct{}

func (localReranker) Rerank(question string, candidateTexts []string) []int {
	qTerms := terms(question)
	qVec := embed(question)
	type item struct {
		idx   int
		score float64
	}
	items := make([]item, len(candidateTexts))
	for i, text := range candidateTexts {
		cTerms := terms(text)
		cVec := embed(text)
		items[i] = item{
			idx:   i,
			score: float64(overlap(qTerms, cTerms))*2 + cosine(qVec, cVec),
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].score > items[j].score
	})
	result := make([]int, len(items))
	for i, it := range items {
		result[i] = it.idx
	}
	return result
}

// ─── Core ────────────────────────────────────────────────────────────────────

// Core is a thread-safe in-memory implementation of brainapi.Core.
type Core struct {
	mu              sync.RWMutex
	now             func() time.Time
	persistencePath string
	initErr         error
	embedder        Embedder
	vectorStore     VectorStore
	synthesizer     Synthesizer
	reranker        Reranker
	topN            int
	bindings        map[brainapi.BindingKey]brainapi.TenantID
	projects        map[brainapi.TenantID]project
	jobs            map[string]brainapi.JobSnapshot
	sources         map[brainapi.TenantID][]brainapi.SourceRef
	docs            map[brainapi.TenantID][]document
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

// ─── Options ─────────────────────────────────────────────────────────────────

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

// WithEmbedder replaces the default localEmbedder with emb.
// A nil emb is silently ignored (the default is kept).
func WithEmbedder(emb Embedder) Option {
	return func(c *Core) {
		if emb != nil {
			c.embedder = emb
		}
	}
}

// WithVectorStore replaces the default inMemoryVectorStore with vs.
// A nil vs is silently ignored (the default is kept).
func WithVectorStore(vs VectorStore) Option {
	return func(c *Core) {
		if vs != nil {
			c.vectorStore = vs
		}
	}
}

// WithSynthesizer replaces the default localSynthesizer with s.
// A nil s is silently ignored (the default is kept).
func WithSynthesizer(s Synthesizer) Option {
	return func(c *Core) {
		if s != nil {
			c.synthesizer = s
		}
	}
}

// WithReranker replaces the default localReranker with r.
// A nil r is silently ignored (the default is kept).
func WithReranker(r Reranker) Option {
	return func(c *Core) {
		if r != nil {
			c.reranker = r
		}
	}
}

// WithTopN sets the number of top-ranked chunks returned by Query.
// The Core fetches topN×3 candidates from the VectorStore and then applies
// the Reranker before selecting the final topN chunks. Values less than 1
// are silently ignored (the default of 3 is kept).
func WithTopN(n int) Option {
	return func(c *Core) {
		if n >= 1 {
			c.topN = n
		}
	}
}

// ─── Constructors ────────────────────────────────────────────────────────────

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
		now:         time.Now,
		embedder:    localEmbedder{},
		vectorStore: newInMemoryVectorStore(),
		synthesizer: localSynthesizer{},
		reranker:    localReranker{},
		topN:        3,
		bindings:    make(map[brainapi.BindingKey]brainapi.TenantID),
		projects:    make(map[brainapi.TenantID]project),
		jobs:        make(map[string]brainapi.JobSnapshot),
		sources:     make(map[brainapi.TenantID][]brainapi.SourceRef),
		docs:        make(map[brainapi.TenantID][]document),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ─── Core methods ─────────────────────────────────────────────────────────────

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
	if err := c.persistLocked(); err != nil {
		delete(c.projects, req.TenantID)
		delete(c.bindings, req.BindingKey)
		return err
	}
	return nil
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
	sourcesBefore := len(c.sources[req.TenantID])
	docsBefore := len(c.docs[req.TenantID])
	chunksBefore := c.vectorStore.Len(req.TenantID)

	doc := documentFrom(req, c.now().UTC())
	newChunks := chunksFrom(req.TenantID, sourcesBefore, doc, chunksBefore, c.embedder)
	c.sources[req.TenantID] = append(c.sources[req.TenantID], req.Source)
	c.docs[req.TenantID] = append(c.docs[req.TenantID], doc)
	c.vectorStore.Add(req.TenantID, newChunks)
	c.jobs[key] = brainapi.JobSnapshot{TenantID: req.TenantID, JobID: req.JobID, Status: brainapi.JobRunning, UpdatedAt: c.now().UTC()}
	if err := c.persistLocked(); err != nil {
		c.sources[req.TenantID] = c.sources[req.TenantID][:sourcesBefore]
		c.docs[req.TenantID] = c.docs[req.TenantID][:docsBefore]
		c.vectorStore.TruncateTo(req.TenantID, chunksBefore)
		delete(c.jobs, key)
		return err
	}
	return nil
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
	oldSnapshot := snapshot
	snapshot.Status = status
	snapshot.ResultRef = resultRef
	snapshot.Error = message
	snapshot.UpdatedAt = c.now().UTC()
	c.jobs[key] = snapshot
	if err := c.persistLocked(); err != nil {
		c.jobs[key] = oldSnapshot
		return err
	}
	return nil
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

// Query retrieves tenant-scoped chunks and returns a grounded answer.
//
// Pipeline:
//  1. VectorStore.Search fetches topN×3 candidates (hybrid BM25+cosine retrieval).
//  2. Reranker.Rerank re-scores and orders the candidates.
//  3. The top-N chunk texts from the reranked list are passed to the Synthesizer.
//  4. Synthesizer.Synthesize returns the answer, grounded spans, and any
//     model-supplemented spans.
//
// All three seams default to local (deterministic, no-network) implementations.
func (c *Core) Query(ctx context.Context, req brainapi.QueryRequest) (brainapi.QueryResponse, error) {
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

	questionTerms := terms(question)
	questionVector := c.embedder.Embed(question)

	// Fetch a candidate set larger than topN so the Reranker has room to
	// reorder. The default localReranker reproduces the VectorStore ordering,
	// so for local tests the final top-N is identical to the previous behaviour.
	candidateN := c.topN * 3
	candidates := c.vectorStore.Search(req.TenantID, questionTerms, questionVector, candidateN)
	if len(candidates) == 0 {
		return brainapi.QueryResponse{Answer: "수집된 근거가 없습니다: " + question, GroundingAvailable: false}, nil
	}

	// Rerank the candidates.
	candidateTexts := make([]string, len(candidates))
	for i, ch := range candidates {
		candidateTexts[i] = ch.text
	}
	ranked := c.reranker.Rerank(question, candidateTexts)

	// Select top-N from the reranked indices, skipping out-of-range values
	// that a custom Reranker implementation might return.
	top := make([]chunk, 0, c.topN)
	for _, idx := range ranked {
		if len(top) >= c.topN {
			break
		}
		if idx < 0 || idx >= len(candidates) {
			continue
		}
		top = append(top, candidates[idx])
	}

	// Synthesize the grounded answer.
	inputs := make([]SynthesisInput, len(top))
	for i, ch := range top {
		inputs[i] = SynthesisInput{Text: ch.text}
	}
	synthesis, err := c.synthesizer.Synthesize(ctx, question, inputs)
	if err != nil {
		return brainapi.QueryResponse{}, err
	}
	return brainapi.QueryResponse{
		Answer:             synthesis.Answer,
		Sources:            uniqueSources(top),
		GroundedSpans:      synthesis.GroundedSpans,
		SupplementedSpans:  synthesis.SupplementedSpans,
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
		results = append(results, brainapi.MetadataResult{ID: string(req.TenantID) + ":metadata:" + strconv.Itoa(i), Title: doc.title, Kind: doc.source.MimeType, FreshAt: doc.freshAt})
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
	oldState := proj.state
	proj.state = state
	c.projects[tenantID] = proj
	if err := c.persistLocked(); err != nil {
		proj.state = oldState
		c.projects[tenantID] = proj
		return err
	}
	return nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

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

// chunksFrom splits doc content into fixed-width word chunks and embeds each
// using emb. The Embedder is threaded in so callers can swap the embedding
// strategy without touching this function.
func chunksFrom(tenantID brainapi.TenantID, sourceIndex int, doc document, offset int, emb Embedder) []chunk {
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
		id := string(tenantID) + ":chunk:" + strconv.Itoa(offset+len(chunks))
		chunks = append(chunks, chunk{
			id: id,
			source: brainapi.Source{
				ID:       string(tenantID) + ":source:" + strconv.Itoa(sourceIndex),
				Title:    doc.title,
				URI:      doc.source.URI,
				TenantID: tenantID,
			},
			text:    text,
			terms:   terms(text),
			vector:  emb.Embed(text),
			freshAt: doc.freshAt,
			kind:    doc.source.MimeType,
		})
	}
	return chunks
}

// rankChunks scores and sorts chunks by overlap*2 + cosine. questionTerms and
// questionVector must be pre-computed by the caller so that they can use the
// configured Embedder rather than the package-level embed function.
func rankChunks(questionTerms map[string]int, questionVector []float64, chunks []chunk) []scoredChunk {
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
		idx := int(stableHash(token) % uint32(embeddingDimensions))
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

// stableHash returns a uint32 FNV-1a hash of token. Returning uint32 (not int)
// ensures stableHash(token) % uint32(embeddingDimensions) is always non-negative,
// preventing a panic on 32-bit platforms where int is 32 bits and converting a
// large uint32 to int yields a negative value.
func stableHash(token string) uint32 {
	var h uint32 = 2166136261
	for _, r := range token {
		h ^= uint32(r)
		h *= 16777619
	}
	return h
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
