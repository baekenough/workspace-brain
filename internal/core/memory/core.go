// Package memory provides an in-memory data core for tests and local demos.
package memory

import (
	"context"
	"math"
	"os"
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
//
// The Chunk type is exported so that external packages can implement VectorStore
// without embedding unexported memory-package types.
type VectorStore interface {
	// Add appends chunks for a tenant (called during Ingest).
	Add(tenantID brainapi.TenantID, chunks []Chunk)
	// Search returns the top-limit ranked chunks matching the pre-embedded query.
	Search(tenantID brainapi.TenantID, questionTerms map[string]int, questionVector []float64, limit int) []Chunk
	// Len returns the current chunk count for a tenant (used before Ingest for rollback).
	Len(tenantID brainapi.TenantID) int
	// TruncateTo removes chunks beyond n for a tenant (Ingest rollback on persist failure).
	TruncateTo(tenantID brainapi.TenantID, n int)
	// Replace overwrites all chunks for a tenant (used during snapshot load).
	Replace(tenantID brainapi.TenantID, chunks []Chunk)
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

// ─── Exported vector record ──────────────────────────────────────────────────

// Chunk is the unit of indexed content returned by VectorStore.Search and
// passed to VectorStore.Add. It is exported so external VectorStore
// implementations (e.g. Qdrant, PostgreSQL pgvector) can satisfy the interface
// without depending on unexported memory-package internals.
//
// Field semantics mirror the former unexported chunk type; all behaviour of the
// in-memory default implementation is byte-for-byte identical after the rename.
type Chunk struct {
	// ID is the deterministic identifier assigned during chunksFrom:
	// format "{tenantID}:chunk:{position}".
	ID string
	// Source is the origin document metadata, including the TenantID that owns
	// this chunk. External VectorStore implementations must populate this on
	// Search so that QueryResponse.Sources is correctly populated.
	Source brainapi.Source
	// Text is the raw text of the chunk, used by the Reranker and Synthesizer.
	Text string
	// Terms is the BM25 term-frequency map for this chunk, computed from Text.
	// In-memory scoring uses Terms for overlap calculation; external stores may
	// ignore it for retrieval but should populate it from stored payload on
	// Search so that the local Reranker can re-score candidates.
	Terms map[string]int
	// Vector is the embedding of Text produced by the configured Embedder.
	Vector []float64
	// FreshAt is the ingest timestamp.
	FreshAt time.Time
	// Kind is the MIME type of the source document.
	Kind string
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
	chunks map[brainapi.TenantID][]Chunk
}

func newInMemoryVectorStore() *inMemoryVectorStore {
	return &inMemoryVectorStore{chunks: make(map[brainapi.TenantID][]Chunk)}
}

// NewInMemoryVectorStore returns a fresh, empty in-memory VectorStore.
// It is exported so external test packages can run the VectorStore contract
// suite against the default implementation alongside their own backends.
func NewInMemoryVectorStore() VectorStore {
	return newInMemoryVectorStore()
}

func (s *inMemoryVectorStore) Add(tenantID brainapi.TenantID, chunks []Chunk) {
	s.chunks[tenantID] = append(s.chunks[tenantID], chunks...)
}

func (s *inMemoryVectorStore) Search(tenantID brainapi.TenantID, questionTerms map[string]int, questionVector []float64, limit int) []Chunk {
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

func (s *inMemoryVectorStore) Replace(tenantID brainapi.TenantID, chunks []Chunk) {
	dst := make([]Chunk, len(chunks))
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
	// sharedBindings maps a project tenant ID to the shared tenant IDs bound
	// to it (see brainapi.SharedKnowledgeCore), in registration order. Only
	// project tenants appear as keys; shared tenants never bind other shared
	// tenants.
	sharedBindings map[brainapi.TenantID][]brainapi.TenantID

	// ─── Persistence seams (see persistence.go) ─────────────────────────────
	//
	// These are per-instance function fields, not package-level vars: fault
	// injection tests overwrite them on a single Core value (e.g.
	// core.marshalSnapshotFn = ...), so parallel tests that each construct
	// their own Core can inject independent failures without any shared
	// mutable state and therefore without any possibility of a data race
	// between concurrently running tests.
	marshalSnapshotFn  func(snapshot) ([]byte, error)
	createAtomicTempFn func(dir, pattern string) (atomicTempFile, error)
	renameAtomicFileFn func(oldpath, newpath string) error
}

type project struct {
	owner     brainapi.Principal
	metadata  map[string]string
	state     brainapi.ProjectState
	createdAt time.Time
	// tier distinguishes an ordinary project tenant (brainapi.TierProject,
	// the default) from a shared knowledge tenant (brainapi.TierShared)
	// provisioned via CreateSharedTenant. The zero value ("") is treated
	// identically to TierProject throughout this package.
	tier brainapi.Tier
}

// isShared reports whether p is a shared knowledge tenant. The zero value of
// tier ("") is treated as a project tenant. persistence.go round-trips tier
// through the snapshot and explicitly normalizes a missing/empty tier to
// brainapi.TierProject on load (see loadSnapshot), so this zero-value
// fallback here is now purely defensive: it only matters for project values
// built without going through CreateProject/CreateSharedTenant, such as
// hand-constructed test fixtures.
func (p project) isShared() bool {
	return p.tier == brainapi.TierShared
}

// Compile-time interface checks: Core must satisfy both the base contract and
// the optional shared-knowledge-tenant extension (#12).
var (
	_ brainapi.Core                = (*Core)(nil)
	_ brainapi.SharedKnowledgeCore = (*Core)(nil)
)

type document struct {
	source  brainapi.SourceRef
	title   string
	content string
	freshAt time.Time
}

type scoredChunk struct {
	chunk Chunk
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
		now:                time.Now,
		embedder:           localEmbedder{},
		vectorStore:        newInMemoryVectorStore(),
		synthesizer:        localSynthesizer{},
		reranker:           localReranker{},
		topN:               3,
		bindings:           make(map[brainapi.BindingKey]brainapi.TenantID),
		projects:           make(map[brainapi.TenantID]project),
		jobs:               make(map[string]brainapi.JobSnapshot),
		sources:            make(map[brainapi.TenantID][]brainapi.SourceRef),
		docs:               make(map[brainapi.TenantID][]document),
		sharedBindings:     make(map[brainapi.TenantID][]brainapi.TenantID),
		marshalSnapshotFn:  defaultMarshalSnapshot,
		createAtomicTempFn: defaultCreateAtomicTemp,
		renameAtomicFileFn: os.Rename,
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
	c.projects[req.TenantID] = project{owner: req.OwnerPrincipal, metadata: cloneMap(req.Metadata), state: brainapi.ProjectOn, createdAt: c.now().UTC(), tier: brainapi.TierProject}
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
	if c.projects[req.TenantID].isShared() {
		return brainapi.E(brainapi.KindUnauthorized, "ingest", "direct ingest into a shared tenant is not allowed; use SharedKnowledgeCore.PromoteSource", nil)
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
	//
	// Scope is [req.TenantID] ∪ [shared tenants bound to req.TenantID] (see
	// scopeTenantsLocked and brainapi.SharedKnowledgeCore). Candidates from
	// every tenant in scope are merged before reranking so relevance ranking
	// treats project and bound-shared content as one pool; strict isolation
	// is preserved because scope never includes another project tenant or an
	// unbound shared tenant.
	candidateN := c.topN * 3
	var candidates []Chunk
	for _, tid := range c.scopeTenantsLocked(req.TenantID) {
		candidates = append(candidates, c.vectorStore.Search(tid, questionTerms, questionVector, candidateN)...)
	}
	if len(candidates) == 0 {
		return brainapi.QueryResponse{Answer: "수집된 근거가 없습니다: " + question, GroundingAvailable: false}, nil
	}

	// Rerank the candidates.
	candidateTexts := make([]string, len(candidates))
	for i, ch := range candidates {
		candidateTexts[i] = ch.Text
	}
	ranked := c.reranker.Rerank(question, candidateTexts)

	// Select top-N from the reranked indices, skipping out-of-range values
	// that a custom Reranker implementation might return.
	top := make([]Chunk, 0, c.topN)
	for _, idx := range ranked {
		if len(top) >= c.topN {
			break
		}
		if idx < 0 || idx >= len(candidates) {
			continue
		}
		top = append(top, candidates[idx])
	}

	// Determine each source's Tier here rather than trusting whatever the
	// VectorStore returned on Chunk.Source.Tier: TierProject for chunks owned
	// by req.TenantID itself, TierShared for chunks contributed by a bound
	// shared tenant (see brainapi.Tier and SharedKnowledgeCore.PromoteSource).
	// This mirrors the postgres core's tier derivation (internal/core/postgres
	// Query) and makes Tier semantics identical across every VectorStore
	// implementation, including ones (e.g. qdrant) whose payload encoding does
	// not round-trip the Tier field.
	for i := range top {
		if top[i].Source.TenantID == req.TenantID {
			top[i].Source.Tier = brainapi.TierProject
		} else {
			top[i].Source.Tier = brainapi.TierShared
		}
	}

	// Synthesize the grounded answer.
	inputs := make([]SynthesisInput, len(top))
	for i, ch := range top {
		inputs[i] = SynthesisInput{Text: ch.Text}
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
	scope := c.scopeTenantsLocked(req.TenantID)
	results := make([]brainapi.MetadataResult, 0, len(c.docs[req.TenantID]))
	for _, tid := range scope {
		for i, doc := range c.docs[tid] {
			if len(queryTerms) > 0 && overlap(queryTerms, terms(doc.title+" "+doc.source.URI+" "+doc.content)) == 0 {
				continue
			}
			results = append(results, brainapi.MetadataResult{ID: string(tid) + ":metadata:" + strconv.Itoa(i), Title: doc.title, Kind: doc.source.MimeType, FreshAt: doc.freshAt})
		}
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

// ─── Shared knowledge tenant operations (brainapi.SharedKnowledgeCore) ───────

// CreateSharedTenant provisions a shared knowledge tenant. It never creates a
// binding-key entry, so the tenant is never resolvable via ResolveBinding.
func (c *Core) CreateSharedTenant(ctx context.Context, req brainapi.CreateSharedTenantRequest) error {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return err
	}
	if req.OwnerPrincipal.ID == "" {
		return brainapi.E(brainapi.KindInvalid, "create_shared_tenant", "owner principal is required", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireReadyLocked("create_shared_tenant"); err != nil {
		return err
	}
	if _, ok := c.projects[req.TenantID]; ok {
		return brainapi.E(brainapi.KindAlreadyExists, "create_shared_tenant", "tenant already exists", nil)
	}
	c.projects[req.TenantID] = project{
		owner:     req.OwnerPrincipal,
		metadata:  cloneMap(req.Metadata),
		state:     brainapi.ProjectOn,
		createdAt: c.now().UTC(),
		tier:      brainapi.TierShared,
	}
	if err := c.persistLocked(); err != nil {
		delete(c.projects, req.TenantID)
		return err
	}
	return nil
}

// BindSharedTenant registers that queries/discovery scoped to
// req.ProjectTenantID must also search req.SharedTenantID.
func (c *Core) BindSharedTenant(ctx context.Context, req brainapi.BindSharedTenantRequest) error {
	_ = ctx
	if err := brainapi.ValidateTenantID(req.ProjectTenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateTenantID(req.SharedTenantID); err != nil {
		return err
	}
	if req.ProjectTenantID == req.SharedTenantID {
		return brainapi.E(brainapi.KindInvalid, "bind_shared_tenant", "project and shared tenant must differ", nil)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireReadyLocked("bind_shared_tenant"); err != nil {
		return err
	}
	projTenant, ok := c.projects[req.ProjectTenantID]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, "bind_shared_tenant", "project tenant not found", nil)
	}
	if projTenant.isShared() {
		return brainapi.E(brainapi.KindInvalid, "bind_shared_tenant", "project tenant must not itself be a shared tenant", nil)
	}
	sharedTenant, ok := c.projects[req.SharedTenantID]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, "bind_shared_tenant", "shared tenant not found", nil)
	}
	if !sharedTenant.isShared() {
		return brainapi.E(brainapi.KindInvalid, "bind_shared_tenant", "target tenant was not created via CreateSharedTenant", nil)
	}
	if c.isBoundLocked(req.ProjectTenantID, req.SharedTenantID) {
		return brainapi.E(brainapi.KindAlreadyExists, "bind_shared_tenant", "binding already exists", nil)
	}
	c.sharedBindings[req.ProjectTenantID] = append(c.sharedBindings[req.ProjectTenantID], req.SharedTenantID)
	if err := c.persistLocked(); err != nil {
		bound := c.sharedBindings[req.ProjectTenantID]
		c.sharedBindings[req.ProjectTenantID] = bound[:len(bound)-1]
		return err
	}
	return nil
}

// SharedBindings returns the shared tenant IDs currently bound to project, in
// registration order. An unknown or unbound project returns an empty slice
// and a nil error.
func (c *Core) SharedBindings(ctx context.Context, project brainapi.TenantID) ([]brainapi.TenantID, error) {
	_ = ctx
	if err := brainapi.ValidateTenantID(project); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("shared_bindings"); err != nil {
		return nil, err
	}
	bound := c.sharedBindings[project]
	out := make([]brainapi.TenantID, len(bound))
	copy(out, bound)
	return out, nil
}

// PromoteSource copies a single already-ingested project source into a shared
// tenant bound to it. It is the only path that adds content to a shared
// tenant; Ingest rejects any TenantID that names a shared tenant outright.
func (c *Core) PromoteSource(ctx context.Context, req brainapi.PromoteRequest) error {
	_ = ctx
	if !req.Admin.HasRole("admin") {
		return brainapi.E(brainapi.KindUnauthorized, "promote_source", "admin role is required", nil)
	}
	if err := brainapi.ValidateTenantID(req.ProjectTenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateTenantID(req.SharedTenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateSourceID(req.SourceID); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.requireReadyLocked("promote_source"); err != nil {
		return err
	}
	projTenant, ok := c.projects[req.ProjectTenantID]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, "promote_source", "project tenant not found", nil)
	}
	if projTenant.isShared() {
		return brainapi.E(brainapi.KindInvalid, "promote_source", "source tenant must be a project tenant", nil)
	}
	sharedTenant, ok := c.projects[req.SharedTenantID]
	if !ok {
		return brainapi.E(brainapi.KindNotFound, "promote_source", "shared tenant not found", nil)
	}
	if !sharedTenant.isShared() {
		return brainapi.E(brainapi.KindInvalid, "promote_source", "destination tenant is not a shared tenant", nil)
	}
	if !c.isBoundLocked(req.ProjectTenantID, req.SharedTenantID) {
		return brainapi.E(brainapi.KindInvalid, "promote_source", "shared tenant is not bound to project tenant", nil)
	}
	doc, ok := c.findDocBySourceIDLocked(req.ProjectTenantID, req.SourceID)
	if !ok {
		return brainapi.E(brainapi.KindNotFound, "promote_source", "source not found", nil)
	}

	sourcesBefore := len(c.sources[req.SharedTenantID])
	docsBefore := len(c.docs[req.SharedTenantID])
	chunksBefore := c.vectorStore.Len(req.SharedTenantID)

	promoted := document{source: doc.source, title: doc.title, content: doc.content, freshAt: c.now().UTC()}
	newChunks := chunksFrom(req.SharedTenantID, sourcesBefore, promoted, chunksBefore, c.embedder)
	for i := range newChunks {
		newChunks[i].Source.Tier = brainapi.TierShared
	}
	c.sources[req.SharedTenantID] = append(c.sources[req.SharedTenantID], promoted.source)
	c.docs[req.SharedTenantID] = append(c.docs[req.SharedTenantID], promoted)
	c.vectorStore.Add(req.SharedTenantID, newChunks)
	if err := c.persistLocked(); err != nil {
		c.sources[req.SharedTenantID] = c.sources[req.SharedTenantID][:sourcesBefore]
		c.docs[req.SharedTenantID] = c.docs[req.SharedTenantID][:docsBefore]
		c.vectorStore.TruncateTo(req.SharedTenantID, chunksBefore)
		return err
	}
	return nil
}

// scopeTenantsLocked returns the tenant IDs to search for a Query/Discover
// call scoped to project: project itself, followed by any shared tenants
// bound to it (see BindSharedTenant) that still exist, are still tier-shared,
// and are still ProjectOn. Must be called with c.mu held (read or write).
func (c *Core) scopeTenantsLocked(project brainapi.TenantID) []brainapi.TenantID {
	bound := c.sharedBindings[project]
	scope := make([]brainapi.TenantID, 0, 1+len(bound))
	scope = append(scope, project)
	for _, sharedID := range bound {
		shared, ok := c.projects[sharedID]
		if !ok || !shared.isShared() || shared.state != brainapi.ProjectOn {
			continue
		}
		scope = append(scope, sharedID)
	}
	return scope
}

// isBoundLocked reports whether shared is bound to project via
// BindSharedTenant. Must be called with c.mu held.
func (c *Core) isBoundLocked(project, shared brainapi.TenantID) bool {
	for _, id := range c.sharedBindings[project] {
		if id == shared {
			return true
		}
	}
	return false
}

// findDocBySourceIDLocked locates the document within tenantID whose
// derived source ID (see AdminListSources) matches sourceID. Must be called
// with c.mu held.
func (c *Core) findDocBySourceIDLocked(tenantID brainapi.TenantID, sourceID string) (document, bool) {
	for i, doc := range c.docs[tenantID] {
		if string(tenantID)+":source:"+strconv.Itoa(i) == sourceID {
			return doc, true
		}
	}
	return document{}, false
}

// ─── Admin read operations ────────────────────────────────────────────────────

// AdminListTenants returns metadata summaries for all provisioned tenants.
// It does not check project state and never requires a tenant GUC.
func (c *Core) AdminListTenants(_ context.Context, _ brainapi.AdminListTenantsRequest) (brainapi.AdminListTenantsResponse, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("admin_list_tenants"); err != nil {
		return brainapi.AdminListTenantsResponse{}, err
	}
	tenants := make([]brainapi.TenantInfo, 0, len(c.projects))
	for id, proj := range c.projects {
		tenants = append(tenants, brainapi.TenantInfo{
			TenantID:  id,
			State:     proj.state,
			OwnerID:   proj.owner.ID,
			CreatedAt: proj.createdAt,
		})
	}
	sort.Slice(tenants, func(i, j int) bool {
		return string(tenants[i].TenantID) < string(tenants[j].TenantID)
	})
	return brainapi.AdminListTenantsResponse{Tenants: tenants}, nil
}

// AdminListBindings returns binding summaries, optionally filtered by tenant.
func (c *Core) AdminListBindings(_ context.Context, req brainapi.AdminListBindingsRequest) (brainapi.AdminListBindingsResponse, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("admin_list_bindings"); err != nil {
		return brainapi.AdminListBindingsResponse{}, err
	}
	bindings := make([]brainapi.BindingInfo, 0, len(c.bindings))
	for key, tid := range c.bindings {
		if req.TenantID != "" && tid != req.TenantID {
			continue
		}
		bindings = append(bindings, brainapi.BindingInfo{BindingKey: key, TenantID: tid})
	}
	sort.Slice(bindings, func(i, j int) bool {
		return string(bindings[i].BindingKey) < string(bindings[j].BindingKey)
	})
	return brainapi.AdminListBindingsResponse{Bindings: bindings}, nil
}

// AdminListSources returns source metadata for a specific tenant without
// checking project state. Chunk content is never returned.
func (c *Core) AdminListSources(_ context.Context, tenantID brainapi.TenantID) (brainapi.AdminListSourcesResponse, error) {
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return brainapi.AdminListSourcesResponse{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("admin_list_sources"); err != nil {
		return brainapi.AdminListSourcesResponse{}, err
	}
	srcs := c.sources[tenantID]
	docs := c.docs[tenantID]
	infos := make([]brainapi.SourceInfo, 0, len(srcs))
	for i, ref := range srcs {
		name := ref.Name
		if name == "" {
			name = ref.URI
		}
		var freshAt time.Time
		if i < len(docs) {
			freshAt = docs[i].freshAt
		}
		infos = append(infos, brainapi.SourceInfo{
			ID:        string(tenantID) + ":source:" + strconv.Itoa(i),
			TenantID:  tenantID,
			Name:      name,
			URI:       ref.URI,
			MimeType:  ref.MimeType,
			CreatedAt: freshAt,
		})
	}
	return brainapi.AdminListSourcesResponse{Sources: infos}, nil
}

// AdminListJobs returns job snapshots for a specific tenant without checking
// project state.
func (c *Core) AdminListJobs(_ context.Context, tenantID brainapi.TenantID) (brainapi.AdminListJobsResponse, error) {
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return brainapi.AdminListJobsResponse{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("admin_list_jobs"); err != nil {
		return brainapi.AdminListJobsResponse{}, err
	}
	prefix := string(tenantID) + "\x00"
	var jobs []brainapi.JobSnapshot
	for key, snap := range c.jobs {
		if strings.HasPrefix(key, prefix) {
			jobs = append(jobs, snap)
		}
	}
	sort.Slice(jobs, func(i, j int) bool {
		return string(jobs[i].JobID) < string(jobs[j].JobID)
	})
	return brainapi.AdminListJobsResponse{Jobs: jobs}, nil
}

// AdminGetJob returns a single job snapshot without checking project state.
func (c *Core) AdminGetJob(_ context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error) {
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	if err := brainapi.ValidateJobID(jobID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.requireReadyLocked("admin_get_job"); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	snap, ok := c.jobs[jobKey(tenantID, jobID)]
	if !ok {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindNotFound, "admin_get_job", "job not found", nil)
	}
	return snap, nil
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
func chunksFrom(tenantID brainapi.TenantID, sourceIndex int, doc document, offset int, emb Embedder) []Chunk {
	words := strings.Fields(doc.content)
	if len(words) == 0 {
		words = []string{doc.title}
	}
	const chunkWords = 48
	chunks := make([]Chunk, 0, (len(words)+chunkWords-1)/chunkWords)
	for start := 0; start < len(words); start += chunkWords {
		end := start + chunkWords
		if end > len(words) {
			end = len(words)
		}
		text := strings.Join(words[start:end], " ")
		id := string(tenantID) + ":chunk:" + strconv.Itoa(offset+len(chunks))
		chunks = append(chunks, Chunk{
			ID: id,
			Source: brainapi.Source{
				ID:       string(tenantID) + ":source:" + strconv.Itoa(sourceIndex),
				Title:    doc.title,
				URI:      doc.source.URI,
				TenantID: tenantID,
				// Tier defaults to project: this is the tier for ordinary
				// Ingest-created chunks. PromoteSource overrides it to
				// TierShared on the copies it writes into a shared tenant.
				Tier: brainapi.TierProject,
			},
			Text:    text,
			Terms:   terms(text),
			Vector:  emb.Embed(text),
			FreshAt: doc.freshAt,
			Kind:    doc.source.MimeType,
		})
	}
	return chunks
}

// rankChunks scores and sorts chunks by overlap*2 + cosine. questionTerms and
// questionVector must be pre-computed by the caller so that they can use the
// configured Embedder rather than the package-level embed function.
func rankChunks(questionTerms map[string]int, questionVector []float64, chunks []Chunk) []scoredChunk {
	ranked := make([]scoredChunk, 0, len(chunks))
	for i, ch := range chunks {
		score := float64(overlap(questionTerms, ch.Terms))*2 + cosine(questionVector, ch.Vector)
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

func topChunks(ranked []scoredChunk, limit int) []Chunk {
	if len(ranked) < limit {
		limit = len(ranked)
	}
	out := make([]Chunk, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, ranked[i].chunk)
	}
	return out
}

func uniqueSources(chunks []Chunk) []brainapi.Source {
	seen := make(map[string]bool)
	sources := make([]brainapi.Source, 0, len(chunks))
	for _, ch := range chunks {
		key := ch.Source.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		sources = append(sources, ch.Source)
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
