-- 002_shared_tenants.sql
-- Shared knowledge tenant support (issue #12, C2 follow-up to the in-memory
-- reference implementation in internal/core/memory/core.go).
--
-- Adds:
--   * tenants.tier: 'project' (default, ordinary CreateProject tenants) or
--     'shared' (CreateSharedTenant tenants, never resolvable via bindings).
--   * shared_bindings: the server-side-only project -> shared tenant
--     relationship registered by SharedKnowledgeCore.BindSharedTenant.
--
-- RLS policies on tenants/sources/chunks are extended so that a connection
-- scoped to a project tenant's GUC (app.tenant_id = <project>) also sees rows
-- owned by any shared tenant currently bound to it. This mirrors the scope
-- computed in Go (see Core.scopeTenantIDs in core.go) so DB-level RLS and
-- application logic stay in lockstep; the admin sentinel ('*') continues to
-- bypass all tenant filtering as before.

-- ─── Tier column ──────────────────────────────────────────────────────────────

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT 'project'
    CHECK (tier IN ('project', 'shared'));

-- ─── Shared bindings ──────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS shared_bindings (
    project_tenant_id   TEXT        NOT NULL REFERENCES tenants(tenant_id) ON DELETE CASCADE,
    shared_tenant_id    TEXT        NOT NULL REFERENCES tenants(tenant_id) ON DELETE CASCADE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_tenant_id, shared_tenant_id)
);

ALTER TABLE shared_bindings ENABLE ROW LEVEL SECURITY;

-- A project tenant may see (and, under the admin GUC, register) its own
-- outgoing bindings; it may never see another project's bindings.
CREATE POLICY shared_binding_isolation ON shared_bindings
    USING (
        current_setting('app.tenant_id', true) = '*'
        OR project_tenant_id = current_setting('app.tenant_id', true)
    );

CREATE INDEX IF NOT EXISTS idx_shared_bindings_project ON shared_bindings(project_tenant_id);
CREATE INDEX IF NOT EXISTS idx_shared_bindings_shared  ON shared_bindings(shared_tenant_id);

-- ─── Extended isolation policies (project ∪ bound-shared scope) ──────────────

DROP POLICY IF EXISTS tenant_isolation ON tenants;
CREATE POLICY tenant_isolation ON tenants
    USING (
        current_setting('app.tenant_id', true) = '*'
        OR tenant_id = current_setting('app.tenant_id', true)
        OR tenant_id IN (
            SELECT shared_tenant_id FROM shared_bindings
            WHERE project_tenant_id = current_setting('app.tenant_id', true)
        )
    );

DROP POLICY IF EXISTS source_isolation ON sources;
CREATE POLICY source_isolation ON sources
    USING (
        current_setting('app.tenant_id', true) = '*'
        OR tenant_id = current_setting('app.tenant_id', true)
        OR tenant_id IN (
            SELECT sb.shared_tenant_id
            FROM shared_bindings sb
            JOIN tenants st ON st.tenant_id = sb.shared_tenant_id
            WHERE sb.project_tenant_id = current_setting('app.tenant_id', true)
              AND st.tier = 'shared'
              AND st.state = 'on'
        )
    );

DROP POLICY IF EXISTS chunk_isolation ON chunks;
CREATE POLICY chunk_isolation ON chunks
    USING (
        current_setting('app.tenant_id', true) = '*'
        OR tenant_id = current_setting('app.tenant_id', true)
        OR tenant_id IN (
            SELECT sb.shared_tenant_id
            FROM shared_bindings sb
            JOIN tenants st ON st.tenant_id = sb.shared_tenant_id
            WHERE sb.project_tenant_id = current_setting('app.tenant_id', true)
              AND st.tier = 'shared'
              AND st.state = 'on'
        )
    );
