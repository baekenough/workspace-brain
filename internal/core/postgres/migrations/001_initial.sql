-- 001_initial.sql
-- workspace-brain PostgreSQL schema with Row-Level Security

-- ─── Tenants / Projects ───────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS tenants (
    tenant_id       TEXT        PRIMARY KEY,
    owner_source    TEXT        NOT NULL DEFAULT '',
    owner_id        TEXT        NOT NULL,
    owner_roles     TEXT[]      NOT NULL DEFAULT '{}',
    metadata        JSONB       NOT NULL DEFAULT '{}',
    state           TEXT        NOT NULL DEFAULT 'on' CHECK (state IN ('on', 'off')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;

-- Tenants are visible only to requests whose app.tenant_id GUC matches.
-- The special value '*' (used by admin / migration paths) bypasses the filter.
CREATE POLICY tenant_isolation ON tenants
    USING (
        current_setting('app.tenant_id', true) = '*'
        OR tenant_id = current_setting('app.tenant_id', true)
    );

-- ─── Bindings ─────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS bindings (
    binding_key     TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants(tenant_id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE bindings ENABLE ROW LEVEL SECURITY;

CREATE POLICY binding_isolation ON bindings
    USING (
        current_setting('app.tenant_id', true) = '*'
        OR tenant_id = current_setting('app.tenant_id', true)
    );

-- ─── Sources ──────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS sources (
    id              BIGSERIAL   PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants(tenant_id) ON DELETE CASCADE,
    uri             TEXT        NOT NULL,
    mime_type       TEXT        NOT NULL DEFAULT '',
    name            TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE sources ENABLE ROW LEVEL SECURITY;

CREATE POLICY source_isolation ON sources
    USING (
        current_setting('app.tenant_id', true) = '*'
        OR tenant_id = current_setting('app.tenant_id', true)
    );

-- ─── Chunks ───────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS chunks (
    id              TEXT        NOT NULL,
    tenant_id       TEXT        NOT NULL REFERENCES tenants(tenant_id) ON DELETE CASCADE,
    source_id       BIGINT      NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    source_uri      TEXT        NOT NULL,
    source_title    TEXT        NOT NULL DEFAULT '',
    text            TEXT        NOT NULL,
    kind            TEXT        NOT NULL DEFAULT '',
    fresh_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, id)
);

ALTER TABLE chunks ENABLE ROW LEVEL SECURITY;

CREATE POLICY chunk_isolation ON chunks
    USING (
        current_setting('app.tenant_id', true) = '*'
        OR tenant_id = current_setting('app.tenant_id', true)
    );

-- ─── Jobs ─────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS jobs (
    tenant_id       TEXT        NOT NULL REFERENCES tenants(tenant_id) ON DELETE CASCADE,
    job_id          TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'running' CHECK (status IN ('accepted','running','completed','failed','check_required')),
    result_ref      TEXT        NOT NULL DEFAULT '',
    error           TEXT        NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, job_id)
);

ALTER TABLE jobs ENABLE ROW LEVEL SECURITY;

CREATE POLICY job_isolation ON jobs
    USING (
        current_setting('app.tenant_id', true) = '*'
        OR tenant_id = current_setting('app.tenant_id', true)
    );

-- ─── Indexes ──────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS idx_sources_tenant ON sources(tenant_id);
CREATE INDEX IF NOT EXISTS idx_chunks_tenant  ON chunks(tenant_id);
CREATE INDEX IF NOT EXISTS idx_jobs_tenant    ON jobs(tenant_id);
