// Package postgres provides a PostgreSQL-backed implementation of brainapi.Core
// with Row-Level Security for tenant isolation.
//
// Each request sets the app.tenant_id GUC on the connection so that RLS
// policies automatically restrict visibility to that tenant's rows. The
// special sentinel '*' is used only during admin operations that must cross
// tenant boundaries (e.g. CreateProject which must check for duplicates before
// the tenant row exists).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

const (
	embeddingDimensions = 16
	topK                = 3
	chunkWords          = 48

	// pgUniqueViolation is the PostgreSQL SQLSTATE for unique_violation.
	pgUniqueViolation = "23505"
)

// Core is a PostgreSQL-backed brainapi.Core implementation.
// It is safe for concurrent use.
type Core struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// Option configures a Core.
type Option func(*Core)

// WithClock injects a deterministic clock (useful in tests).
func WithClock(clock func() time.Time) Option {
	return func(c *Core) {
		if clock != nil {
			c.now = clock
		}
	}
}

// New opens a pgxpool connection to dsn, runs migrations, and returns a ready Core.
// The caller is responsible for calling Close when the Core is no longer needed.
func New(ctx context.Context, dsn string, opts ...Option) (*Core, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres.New: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres.New: ping: %w", err)
	}

	// Run migrations using a single dedicated connection so that the
	// transaction wrapping in migrate() sees schema changes immediately.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres.New: acquire for migration: %w", err)
	}
	migrErr := migrate(ctx, conn.Conn())
	conn.Release()
	if migrErr != nil {
		pool.Close()
		return nil, migrErr
	}

	c := &Core{pool: pool, now: time.Now}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Close releases the connection pool.
func (c *Core) Close() {
	c.pool.Close()
}

// ─── brainapi.Core implementation ────────────────────────────────────────────

// ResolveBinding returns the TenantID for a surface-neutral binding key.
func (c *Core) ResolveBinding(ctx context.Context, binding brainapi.BindingKey) (brainapi.TenantID, error) {
	if err := brainapi.ValidateBindingKey(binding); err != nil {
		return "", err
	}
	// Binding lookup is cross-tenant: use '*' so RLS does not filter it away.
	conn, err := c.acquire(ctx)
	if err != nil {
		return "", wrapInternal("resolve_binding", err)
	}
	defer conn.Release()

	if err := setAdminGUC(ctx, conn.Conn()); err != nil {
		return "", wrapInternal("resolve_binding", err)
	}

	var tenantID string
	err = conn.QueryRow(ctx,
		`SELECT tenant_id FROM bindings WHERE binding_key = $1`, string(binding),
	).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", brainapi.E(brainapi.KindNotFound, "resolve_binding", "binding not found", nil)
	}
	if err != nil {
		return "", wrapInternal("resolve_binding", err)
	}
	return brainapi.TenantID(tenantID), nil
}

// CreateProject atomically provisions a tenant and its initial binding.
func (c *Core) CreateProject(ctx context.Context, req brainapi.CreateProjectRequest) error {
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateBindingKey(req.BindingKey); err != nil {
		return err
	}
	if req.OwnerPrincipal.ID == "" {
		return brainapi.E(brainapi.KindInvalid, "create_project", "owner principal is required", nil)
	}

	conn, err := c.acquire(ctx)
	if err != nil {
		return wrapInternal("create_project", err)
	}
	defer conn.Release()

	// Admin GUC lets RLS pass for both insert and duplicate check.
	if err := setAdminGUC(ctx, conn.Conn()); err != nil {
		return wrapInternal("create_project", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return wrapInternal("create_project", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	roles := req.OwnerPrincipal.Roles
	if roles == nil {
		roles = []string{}
	}
	metadata := marshalMetadata(req.Metadata)

	_, err = tx.Exec(ctx,
		`INSERT INTO tenants (tenant_id, owner_source, owner_id, owner_roles, metadata, state)
		 VALUES ($1, $2, $3, $4, $5, 'on')`,
		string(req.TenantID),
		req.OwnerPrincipal.Source,
		req.OwnerPrincipal.ID,
		roles,
		metadata,
	)
	if err != nil {
		if isPGUniqueViolation(err) {
			return brainapi.E(brainapi.KindAlreadyExists, "create_project", "tenant already exists", nil)
		}
		return wrapInternal("create_project", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO bindings (binding_key, tenant_id) VALUES ($1, $2)`,
		string(req.BindingKey),
		string(req.TenantID),
	)
	if err != nil {
		if isPGUniqueViolation(err) {
			return brainapi.E(brainapi.KindAlreadyExists, "create_project", "binding already exists", nil)
		}
		return wrapInternal("create_project", err)
	}

	return mapErr("create_project", tx.Commit(ctx))
}

// Ingest stores source metadata, creates text chunks, and records a running job.
func (c *Core) Ingest(ctx context.Context, req brainapi.IngestRequest) error {
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return err
	}
	if err := brainapi.ValidateJobID(req.JobID); err != nil {
		return err
	}
	if strings.TrimSpace(req.Source.URI) == "" {
		return brainapi.E(brainapi.KindInvalid, "ingest", "source uri is required", nil)
	}

	conn, err := c.acquire(ctx)
	if err != nil {
		return wrapInternal("ingest", err)
	}
	defer conn.Release()

	if err := setTenantGUC(ctx, conn.Conn(), req.TenantID); err != nil {
		return wrapInternal("ingest", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return wrapInternal("ingest", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Verify tenant exists and is on (RLS enforces visibility).
	state, err := fetchProjectState(ctx, tx, req.TenantID)
	if err != nil {
		return err
	}
	if state == string(brainapi.ProjectOff) {
		return brainapi.E(brainapi.KindConflict, "ingest", "project is off", nil)
	}

	// Insert job — unique constraint catches duplicates.
	_, err = tx.Exec(ctx,
		`INSERT INTO jobs (tenant_id, job_id, status, updated_at)
		 VALUES ($1, $2, 'running', $3)`,
		string(req.TenantID), string(req.JobID), c.now().UTC(),
	)
	if err != nil {
		if isPGUniqueViolation(err) {
			return brainapi.E(brainapi.KindAlreadyExists, "ingest", "job already exists", nil)
		}
		return wrapInternal("ingest", err)
	}

	// Insert source row, get back its ID for FK reference.
	name := req.Source.Name
	if name == "" {
		name = req.Source.URI
	}
	var sourceID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO sources (tenant_id, uri, mime_type, name)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		string(req.TenantID), req.Source.URI, req.Source.MimeType, name,
	).Scan(&sourceID)
	if err != nil {
		return wrapInternal("ingest", err)
	}

	// Derive content and build chunks.
	content := strings.TrimSpace(req.Metadata["content"])
	if content == "" {
		content = strings.TrimSpace(name + " " + req.Source.URI + " " + req.Source.MimeType)
	}
	freshAt := c.now().UTC()
	chunks := buildChunks(req.TenantID, sourceID, req.Source.URI, name, req.Source.MimeType, content, freshAt)

	// Bulk-insert chunks.
	for i, ch := range chunks {
		_, err = tx.Exec(ctx,
			`INSERT INTO chunks (id, tenant_id, source_id, source_uri, source_title, text, kind, fresh_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			ch.id,
			string(req.TenantID),
			sourceID,
			ch.sourceURI,
			ch.sourceTitle,
			ch.text,
			ch.kind,
			freshAt,
		)
		if err != nil {
			return fmt.Errorf("postgres ingest: insert chunk %d: %w", i, wrapInternal("ingest", err))
		}
	}

	return mapErr("ingest", tx.Commit(ctx))
}

// JobStatus returns tenant-scoped job state.
func (c *Core) JobStatus(ctx context.Context, tenantID brainapi.TenantID, jobID brainapi.JobID) (brainapi.JobSnapshot, error) {
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return brainapi.JobSnapshot{}, err
	}
	if err := brainapi.ValidateJobID(jobID); err != nil {
		return brainapi.JobSnapshot{}, err
	}

	conn, err := c.acquire(ctx)
	if err != nil {
		return brainapi.JobSnapshot{}, wrapInternal("job_status", err)
	}
	defer conn.Release()

	if err := setTenantGUC(ctx, conn.Conn(), tenantID); err != nil {
		return brainapi.JobSnapshot{}, wrapInternal("job_status", err)
	}

	var status, resultRef, jobErr string
	var updatedAt time.Time
	err = conn.QueryRow(ctx,
		`SELECT status, result_ref, error, updated_at
		 FROM jobs WHERE tenant_id = $1 AND job_id = $2`,
		string(tenantID), string(jobID),
	).Scan(&status, &resultRef, &jobErr, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return brainapi.JobSnapshot{}, brainapi.E(brainapi.KindNotFound, "job_status", "job not found", nil)
	}
	if err != nil {
		return brainapi.JobSnapshot{}, wrapInternal("job_status", err)
	}
	return brainapi.JobSnapshot{
		TenantID:  tenantID,
		JobID:     jobID,
		Status:    brainapi.JobStatus(status),
		ResultRef: resultRef,
		Error:     jobErr,
		UpdatedAt: updatedAt,
	}, nil
}

// Query retrieves tenant-scoped chunks and returns a grounded answer.
// Similarity is computed in Go using the same bag-of-terms + cosine formula as
// the in-memory core (no pgvector required).
func (c *Core) Query(ctx context.Context, req brainapi.QueryRequest) (brainapi.QueryResponse, error) {
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return brainapi.QueryResponse{}, err
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		return brainapi.QueryResponse{}, brainapi.E(brainapi.KindInvalid, "query", "question is required", nil)
	}

	conn, err := c.acquire(ctx)
	if err != nil {
		return brainapi.QueryResponse{}, wrapInternal("query", err)
	}
	defer conn.Release()

	if err := setTenantGUC(ctx, conn.Conn(), req.TenantID); err != nil {
		return brainapi.QueryResponse{}, wrapInternal("query", err)
	}

	// Verify project exists and is on.
	state, err := fetchProjectStateFromPool(ctx, conn.Conn(), req.TenantID)
	if err != nil {
		return brainapi.QueryResponse{}, err
	}
	if state == string(brainapi.ProjectOff) {
		return brainapi.QueryResponse{}, brainapi.E(brainapi.KindConflict, "query", "project is off", nil)
	}

	rows, err := conn.Query(ctx,
		`SELECT id, source_id, source_uri, source_title, text, kind, fresh_at
		 FROM chunks WHERE tenant_id = $1`,
		string(req.TenantID),
	)
	if err != nil {
		return brainapi.QueryResponse{}, wrapInternal("query", err)
	}
	defer rows.Close()

	type dbChunk struct {
		id          string
		sourceID    int64
		sourceURI   string
		sourceTitle string
		text        string
		kind        string
		freshAt     time.Time
	}

	var dbChunks []dbChunk
	for rows.Next() {
		var ch dbChunk
		if err := rows.Scan(&ch.id, &ch.sourceID, &ch.sourceURI, &ch.sourceTitle, &ch.text, &ch.kind, &ch.freshAt); err != nil {
			return brainapi.QueryResponse{}, wrapInternal("query", err)
		}
		dbChunks = append(dbChunks, ch)
	}
	if err := rows.Err(); err != nil {
		return brainapi.QueryResponse{}, wrapInternal("query", err)
	}

	if len(dbChunks) == 0 {
		return brainapi.QueryResponse{Answer: "수집된 근거가 없습니다: " + question, GroundingAvailable: false}, nil
	}

	// Rank using the same algorithm as the in-memory core.
	qTerms := termFreq(question)
	qVec := embedText(question)

	type ranked struct {
		ch    dbChunk
		score float64
		order int
	}
	scores := make([]ranked, len(dbChunks))
	for i, ch := range dbChunks {
		cTerms := termFreq(ch.text)
		cVec := embedText(ch.text)
		score := float64(overlap(qTerms, cTerms))*2 + cosine(qVec, cVec)
		scores[i] = ranked{ch: ch, score: score, order: i}
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return scores[i].order < scores[j].order
		}
		return scores[i].score > scores[j].score
	})
	limit := topK
	if len(scores) < limit {
		limit = len(scores)
	}
	top := scores[:limit]

	// Build response.
	seenSrc := make(map[string]bool)
	sources := make([]brainapi.Source, 0, limit)
	spans := make([]string, 0, limit)
	for _, r := range top {
		srcKey := strconv.FormatInt(r.ch.sourceID, 10)
		if !seenSrc[srcKey] {
			seenSrc[srcKey] = true
			sources = append(sources, brainapi.Source{
				ID:       string(req.TenantID) + ":source:" + srcKey,
				Title:    r.ch.sourceTitle,
				URI:      r.ch.sourceURI,
				TenantID: req.TenantID,
			})
		}
		spans = append(spans, r.ch.text)
	}

	return brainapi.QueryResponse{
		Answer:             "근거 기반 응답: " + top[0].ch.text,
		Sources:            sources,
		GroundedSpans:      spans,
		GroundingAvailable: true,
	}, nil
}

// Discover returns tenant-scoped source metadata (no chunk content).
func (c *Core) Discover(ctx context.Context, req brainapi.DiscoverRequest) (brainapi.DiscoverResponse, error) {
	if err := brainapi.ValidateTenantID(req.TenantID); err != nil {
		return brainapi.DiscoverResponse{}, err
	}

	conn, err := c.acquire(ctx)
	if err != nil {
		return brainapi.DiscoverResponse{}, wrapInternal("discover", err)
	}
	defer conn.Release()

	if err := setTenantGUC(ctx, conn.Conn(), req.TenantID); err != nil {
		return brainapi.DiscoverResponse{}, wrapInternal("discover", err)
	}

	// Verify project exists and is on.
	state, err := fetchProjectStateFromPool(ctx, conn.Conn(), req.TenantID)
	if err != nil {
		return brainapi.DiscoverResponse{}, err
	}
	if state == string(brainapi.ProjectOff) {
		return brainapi.DiscoverResponse{}, brainapi.E(brainapi.KindConflict, "discover", "project is off", nil)
	}

	rows, err := conn.Query(ctx,
		`SELECT id, name, mime_type, created_at FROM sources WHERE tenant_id = $1 ORDER BY id`,
		string(req.TenantID),
	)
	if err != nil {
		return brainapi.DiscoverResponse{}, wrapInternal("discover", err)
	}

	// Drain source rows into a slice so we can close the cursor before issuing
	// additional queries (pgx does not allow two active result sets on one conn).
	type srcRow struct {
		id        int64
		name      string
		mimeType  string
		createdAt time.Time
	}
	var srcs []srcRow
	for rows.Next() {
		var r srcRow
		if err := rows.Scan(&r.id, &r.name, &r.mimeType, &r.createdAt); err != nil {
			rows.Close()
			return brainapi.DiscoverResponse{}, wrapInternal("discover", err)
		}
		srcs = append(srcs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return brainapi.DiscoverResponse{}, wrapInternal("discover", err)
	}

	queryTerms := termFreq(req.Query)
	var results []brainapi.MetadataResult
	for _, r := range srcs {
		// If a query is provided, filter by term overlap against name then content.
		if len(queryTerms) > 0 {
			docTerms := termFreq(r.name)
			if overlap(queryTerms, docTerms) == 0 {
				if !matchesChunkContent(ctx, conn.Conn(), req.TenantID, r.id, queryTerms) {
					continue
				}
			}
		}
		results = append(results, brainapi.MetadataResult{
			ID:      string(req.TenantID) + ":metadata:" + strconv.FormatInt(r.id, 10),
			Title:   r.name,
			Kind:    r.mimeType,
			FreshAt: r.createdAt,
		})
	}

	return brainapi.DiscoverResponse{Results: results}, nil
}

// SetProjectState freezes or enables a tenant.
func (c *Core) SetProjectState(ctx context.Context, tenantID brainapi.TenantID, state brainapi.ProjectState) error {
	if err := brainapi.ValidateTenantID(tenantID); err != nil {
		return err
	}
	if state != brainapi.ProjectOn && state != brainapi.ProjectOff {
		return brainapi.E(brainapi.KindInvalid, "set_project_state", "unknown project state", nil)
	}

	conn, err := c.acquire(ctx)
	if err != nil {
		return wrapInternal("set_project_state", err)
	}
	defer conn.Release()

	// Use admin GUC so the UPDATE is not filtered by tenant RLS.
	if err := setAdminGUC(ctx, conn.Conn()); err != nil {
		return wrapInternal("set_project_state", err)
	}

	tag, err := conn.Exec(ctx,
		`UPDATE tenants SET state = $1 WHERE tenant_id = $2`,
		string(state), string(tenantID),
	)
	if err != nil {
		return wrapInternal("set_project_state", err)
	}
	if tag.RowsAffected() == 0 {
		return brainapi.E(brainapi.KindNotFound, "set_project_state", "tenant not found", nil)
	}
	return nil
}

// ─── GUC helpers ─────────────────────────────────────────────────────────────

// setTenantGUC sets app.tenant_id on the connection so that all subsequent
// queries in the same connection are filtered by the RLS policies.
func setTenantGUC(ctx context.Context, conn *pgx.Conn, tenantID brainapi.TenantID) error {
	_, err := conn.Exec(ctx, `SET LOCAL app.tenant_id = $1`, string(tenantID))
	if err != nil {
		// SET LOCAL requires an active transaction; fall back to SET.
		_, err = conn.Exec(ctx, fmt.Sprintf(`SET app.tenant_id = '%s'`, escapeSQLString(string(tenantID))))
	}
	return err
}

// setAdminGUC sets app.tenant_id to the sentinel '*' which the RLS policies
// interpret as "bypass tenant filter".
func setAdminGUC(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, `SET app.tenant_id = '*'`)
	return err
}

// escapeSQLString escapes single quotes to prevent SQL injection in the
// fallback SET statement (only used when SET LOCAL is unavailable outside a tx).
func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// ─── Project state helpers ────────────────────────────────────────────────────

// fetchProjectState queries the tenant state inside a transaction.
// Returns KindNotFound if the tenant does not exist.
func fetchProjectState(ctx context.Context, tx pgx.Tx, tenantID brainapi.TenantID) (string, error) {
	var state string
	err := tx.QueryRow(ctx,
		`SELECT state FROM tenants WHERE tenant_id = $1`, string(tenantID),
	).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", brainapi.E(brainapi.KindNotFound, "ingest", "tenant not found", nil)
	}
	if err != nil {
		return "", wrapInternal("ingest", err)
	}
	return state, nil
}

// fetchProjectStateFromPool queries tenant state directly on a *pgx.Conn.
func fetchProjectStateFromPool(ctx context.Context, conn *pgx.Conn, tenantID brainapi.TenantID) (string, error) {
	var state string
	err := conn.QueryRow(ctx,
		`SELECT state FROM tenants WHERE tenant_id = $1`, string(tenantID),
	).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", brainapi.E(brainapi.KindNotFound, "query", "tenant not found", nil)
	}
	if err != nil {
		return "", wrapInternal("query", err)
	}
	return state, nil
}

// matchesChunkContent checks whether any chunk for sourceID contains any of queryTerms.
func matchesChunkContent(ctx context.Context, conn *pgx.Conn, tenantID brainapi.TenantID, sourceID int64, queryTerms map[string]int) bool {
	rows, err := conn.Query(ctx,
		`SELECT text FROM chunks WHERE tenant_id = $1 AND source_id = $2`,
		string(tenantID), sourceID,
	)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			continue
		}
		if overlap(queryTerms, termFreq(text)) > 0 {
			return true
		}
	}
	return false
}

// ─── Pool helper ─────────────────────────────────────────────────────────────

func (c *Core) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	return c.pool.Acquire(ctx)
}

// ─── Error helpers ────────────────────────────────────────────────────────────

func isPGUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

func wrapInternal(op string, err error) error {
	if err == nil {
		return nil
	}
	return brainapi.E(brainapi.KindInternal, op, err.Error(), err)
}

func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	if isPGUniqueViolation(err) {
		return brainapi.E(brainapi.KindAlreadyExists, op, "duplicate key", err)
	}
	return wrapInternal(op, err)
}

// ─── Metadata serialisation ───────────────────────────────────────────────────

func marshalMetadata(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	var sb strings.Builder
	sb.WriteByte('{')
	i := 0
	for k, v := range m {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"`)
		sb.WriteString(jsonEscape(k))
		sb.WriteString(`":"`)
		sb.WriteString(jsonEscape(v))
		sb.WriteString(`"`)
		i++
	}
	sb.WriteByte('}')
	return sb.String()
}

func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// ─── Text processing (mirrors internal/core/memory) ──────────────────────────

type textChunk struct {
	id          string
	sourceID    int64
	sourceURI   string
	sourceTitle string
	text        string
	kind        string
}

func buildChunks(tenantID brainapi.TenantID, sourceID int64, uri, title, mimeType, content string, _ time.Time) []textChunk {
	words := strings.Fields(content)
	if len(words) == 0 {
		words = []string{title}
	}
	// Scope chunk IDs to (tenantID, sourceID, index) so that multiple Ingest
	// calls for the same tenant never produce colliding primary keys.
	srcKey := strconv.FormatInt(sourceID, 10)
	chunks := make([]textChunk, 0, (len(words)+chunkWords-1)/chunkWords)
	for start := 0; start < len(words); start += chunkWords {
		end := start + chunkWords
		if end > len(words) {
			end = len(words)
		}
		text := strings.Join(words[start:end], " ")
		id := string(tenantID) + ":chunk:" + srcKey + ":" + strconv.Itoa(len(chunks))
		chunks = append(chunks, textChunk{
			id:          id,
			sourceID:    sourceID,
			sourceURI:   uri,
			sourceTitle: title,
			text:        text,
			kind:        mimeType,
		})
	}
	return chunks
}

func termFreq(text string) map[string]int {
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
	for token, lc := range left {
		if rc := right[token]; rc > 0 {
			if lc < rc {
				count += lc
			} else {
				count += rc
			}
		}
	}
	return count
}

func embedText(text string) []float64 {
	vec := make([]float64, embeddingDimensions)
	for token, count := range termFreq(text) {
		idx := int(stableHash(token) % uint32(embeddingDimensions))
		vec[idx] += float64(count)
	}
	return vec
}

func cosine(left, right []float64) float64 {
	var dot, lNorm, rNorm float64
	for i := range left {
		dot += left[i] * right[i]
		lNorm += left[i] * left[i]
		rNorm += right[i] * right[i]
	}
	if lNorm == 0 || rNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(lNorm) * math.Sqrt(rNorm))
}

func stableHash(token string) uint32 {
	var h uint32 = 2166136261
	for _, r := range token {
		h ^= uint32(r)
		h *= 16777619
	}
	return h
}
