// Package coretest provides a reusable black-box contract test suite for any
// implementation of brainapi.Core. Tests operate exclusively through the public
// interface so the same suite can validate in-memory, PostgreSQL, Qdrant, or any
// other backend without touching unexported internals.
package coretest

import (
	"context"
	"testing"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// RunContractSuite runs the brainapi.Core behavioral contract tests against the
// implementation returned by newCore. newCore must return a fresh, empty Core on
// every call so that sub-tests are independent and can run in parallel.
//
// Assumptions about the implementation under test:
//   - Ingest processing is synchronous: chunks are searchable immediately after
//     Ingest returns without error.
//   - Duplicate detection (AlreadyExists) is scoped to the same Core instance.
func RunContractSuite(t *testing.T, newCore func() brainapi.Core) {
	t.Helper()
	ctx := context.Background()

	// ─── CreateProject / ResolveBinding ─────────────────────────────────────

	t.Run("create_project_and_resolve_binding", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		req := brainapi.CreateProjectRequest{
			TenantID:       "contract-tenant-a",
			BindingKey:     "api:space:CTA",
			OwnerPrincipal: brainapi.Principal{ID: "owner-1"},
		}
		if err := core.CreateProject(ctx, req); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		tenantID, err := core.ResolveBinding(ctx, "api:space:CTA")
		if err != nil {
			t.Fatalf("ResolveBinding: %v", err)
		}
		if tenantID != "contract-tenant-a" {
			t.Fatalf("got tenant %q, want contract-tenant-a", tenantID)
		}
	})

	t.Run("create_project_duplicate_tenant", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		req := brainapi.CreateProjectRequest{
			TenantID:       "contract-dup-tenant",
			BindingKey:     "api:space:CDUP",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}
		if err := core.CreateProject(ctx, req); err != nil {
			t.Fatalf("first CreateProject: %v", err)
		}
		err := core.CreateProject(ctx, req)
		if !brainapi.IsKind(err, brainapi.KindAlreadyExists) {
			t.Fatalf("duplicate tenant: want AlreadyExists, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("create_project_duplicate_binding", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		req := brainapi.CreateProjectRequest{
			TenantID:       "contract-dup-binding-a",
			BindingKey:     "api:space:CDBA",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}
		if err := core.CreateProject(ctx, req); err != nil {
			t.Fatalf("first CreateProject: %v", err)
		}
		err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-dup-binding-b",
			BindingKey:     "api:space:CDBA", // same binding, different tenant
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		})
		if !brainapi.IsKind(err, brainapi.KindAlreadyExists) {
			t.Fatalf("duplicate binding: want AlreadyExists, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("resolve_missing_binding_returns_not_found", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		_, err := core.ResolveBinding(ctx, "api:space:MISSING")
		if !brainapi.IsKind(err, brainapi.KindNotFound) {
			t.Fatalf("missing binding: want NotFound, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	// ─── Ingest / JobStatus ─────────────────────────────────────────────────

	t.Run("ingest_creates_running_job", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-ingest-tenant",
			BindingKey:     "api:space:CIT",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "contract-ingest-tenant",
			JobID:    "contract-job-1",
			Source:   brainapi.SourceRef{URI: "file://ingest.txt", Name: "ingest.txt"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		status, err := core.JobStatus(ctx, "contract-ingest-tenant", "contract-job-1")
		if err != nil {
			t.Fatalf("JobStatus: %v", err)
		}
		if status.Status != brainapi.JobRunning {
			t.Fatalf("job status = %q, want Running", status.Status)
		}
		if status.TenantID != "contract-ingest-tenant" {
			t.Fatalf("job tenant = %q, want contract-ingest-tenant", status.TenantID)
		}
	})

	t.Run("ingest_duplicate_job_returns_already_exists", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-dupjob-tenant",
			BindingKey:     "api:space:CDJ",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		req := brainapi.IngestRequest{
			TenantID: "contract-dupjob-tenant",
			JobID:    "contract-job-dup",
			Source:   brainapi.SourceRef{URI: "file://dup.txt"},
		}
		if err := core.Ingest(ctx, req); err != nil {
			t.Fatalf("first Ingest: %v", err)
		}
		err := core.Ingest(ctx, req)
		if !brainapi.IsKind(err, brainapi.KindAlreadyExists) {
			t.Fatalf("duplicate job: want AlreadyExists, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("job_status_not_found_for_missing_job", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-nojob-tenant",
			BindingKey:     "api:space:CNJ",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		_, err := core.JobStatus(ctx, "contract-nojob-tenant", "does-not-exist")
		if !brainapi.IsKind(err, brainapi.KindNotFound) {
			t.Fatalf("missing job: want NotFound, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	// ─── Query ──────────────────────────────────────────────────────────────

	t.Run("query_without_sources_returns_no_grounding", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-noground-tenant",
			BindingKey:     "api:space:CNG",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		resp, err := core.Query(ctx, brainapi.QueryRequest{
			TenantID: "contract-noground-tenant",
			Question: "what is the meaning of everything",
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if resp.GroundingAvailable {
			t.Fatalf("GroundingAvailable = true, want false when no sources ingested")
		}
	})

	t.Run("query_with_sources_returns_grounding", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-ground-tenant",
			BindingKey:     "api:space:CGT",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "contract-ground-tenant",
			JobID:    "contract-ground-job",
			Source:   brainapi.SourceRef{URI: "file://knowledge.txt", Name: "knowledge.txt"},
			Metadata: map[string]string{"content": "contract knowledge base alpha beta gamma"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		resp, err := core.Query(ctx, brainapi.QueryRequest{
			TenantID: "contract-ground-tenant",
			Question: "knowledge alpha",
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !resp.GroundingAvailable {
			t.Fatalf("GroundingAvailable = false, want true after ingest")
		}
		if len(resp.Sources) == 0 {
			t.Fatalf("Sources is empty, want at least one source")
		}
		for _, src := range resp.Sources {
			if src.TenantID != "contract-ground-tenant" {
				t.Fatalf("source TenantID = %q, want contract-ground-tenant", src.TenantID)
			}
		}
	})

	t.Run("query_tenant_isolation", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		// Tenant A ingests content with "secret" keyword.
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-iso-a",
			BindingKey:     "api:space:CISA",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject A: %v", err)
		}
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-iso-b",
			BindingKey:     "api:space:CISB",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject B: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "contract-iso-a",
			JobID:    "contract-iso-job-a",
			Source:   brainapi.SourceRef{URI: "file://secret-a.txt"},
			Metadata: map[string]string{"content": "secret information for tenant a only"},
		}); err != nil {
			t.Fatalf("Ingest A: %v", err)
		}
		// Query from tenant B must not see tenant A's data.
		resp, err := core.Query(ctx, brainapi.QueryRequest{
			TenantID: "contract-iso-b",
			Question: "secret",
		})
		if err != nil {
			t.Fatalf("Query B: %v", err)
		}
		if resp.GroundingAvailable {
			t.Fatalf("tenant B query returned grounding from tenant A's data: %+v", resp)
		}
		for _, src := range resp.Sources {
			if src.TenantID == "contract-iso-a" {
				t.Fatalf("tenant isolation broken: tenant B query returned source from tenant A")
			}
		}
	})

	// ─── Discover ───────────────────────────────────────────────────────────

	t.Run("discover_returns_all_ingested_results", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-disc-tenant",
			BindingKey:     "api:space:CDT",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		for i, uri := range []string{"file://doc-alpha.txt", "file://doc-beta.txt"} {
			jobID := brainapi.JobID("contract-disc-job-" + string(rune('1'+i)))
			if err := core.Ingest(ctx, brainapi.IngestRequest{
				TenantID: "contract-disc-tenant",
				JobID:    jobID,
				Source:   brainapi.SourceRef{URI: uri, Name: uri},
			}); err != nil {
				t.Fatalf("Ingest %s: %v", uri, err)
			}
		}
		resp, err := core.Discover(ctx, brainapi.DiscoverRequest{TenantID: "contract-disc-tenant"})
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(resp.Results) != 2 {
			t.Fatalf("Discover got %d results, want 2", len(resp.Results))
		}
	})

	t.Run("discover_with_query_filters_results", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-discf-tenant",
			BindingKey:     "api:space:CDFT",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "contract-discf-tenant",
			JobID:    "contract-discf-job-1",
			Source:   brainapi.SourceRef{URI: "file://relevant.txt", Name: "relevant.txt"},
			Metadata: map[string]string{"content": "needle in a haystack document"},
		}); err != nil {
			t.Fatalf("Ingest relevant: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "contract-discf-tenant",
			JobID:    "contract-discf-job-2",
			Source:   brainapi.SourceRef{URI: "file://irrelevant.txt", Name: "irrelevant.txt"},
			Metadata: map[string]string{"content": "unrelated content about other things"},
		}); err != nil {
			t.Fatalf("Ingest irrelevant: %v", err)
		}
		resp, err := core.Discover(ctx, brainapi.DiscoverRequest{
			TenantID: "contract-discf-tenant",
			Query:    "needle",
		})
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		if len(resp.Results) != 1 {
			t.Fatalf("Discover with filter got %d results, want 1: %+v", len(resp.Results), resp.Results)
		}
		if resp.Results[0].Title != "relevant.txt" {
			t.Fatalf("Discover returned wrong result title %q, want relevant.txt", resp.Results[0].Title)
		}
	})

	// ─── SetProjectState ─────────────────────────────────────────────────────

	t.Run("project_off_blocks_query", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-off-query-tenant",
			BindingKey:     "api:space:COQT",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.SetProjectState(ctx, "contract-off-query-tenant", brainapi.ProjectOff); err != nil {
			t.Fatalf("SetProjectState Off: %v", err)
		}
		_, err := core.Query(ctx, brainapi.QueryRequest{
			TenantID: "contract-off-query-tenant",
			Question: "anything",
		})
		if !brainapi.IsKind(err, brainapi.KindConflict) {
			t.Fatalf("Query on off project: want Conflict, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("project_off_blocks_discover", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-off-disc-tenant",
			BindingKey:     "api:space:CODT",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.SetProjectState(ctx, "contract-off-disc-tenant", brainapi.ProjectOff); err != nil {
			t.Fatalf("SetProjectState Off: %v", err)
		}
		_, err := core.Discover(ctx, brainapi.DiscoverRequest{TenantID: "contract-off-disc-tenant"})
		if !brainapi.IsKind(err, brainapi.KindConflict) {
			t.Fatalf("Discover on off project: want Conflict, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("project_off_then_on_allows_query", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-toggle-tenant",
			BindingKey:     "api:space:CTT",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.SetProjectState(ctx, "contract-toggle-tenant", brainapi.ProjectOff); err != nil {
			t.Fatalf("SetProjectState Off: %v", err)
		}
		if err := core.SetProjectState(ctx, "contract-toggle-tenant", brainapi.ProjectOn); err != nil {
			t.Fatalf("SetProjectState On: %v", err)
		}
		// Query must succeed (even with no sources — just no grounding).
		resp, err := core.Query(ctx, brainapi.QueryRequest{
			TenantID: "contract-toggle-tenant",
			Question: "test",
		})
		if err != nil {
			t.Fatalf("Query after re-enable: %v", err)
		}
		// No sources were ingested, so grounding must be unavailable.
		if resp.GroundingAvailable {
			t.Fatalf("GroundingAvailable = true with no sources, want false")
		}
	})

	// ─── Validation ──────────────────────────────────────────────────────────

	t.Run("validation_invalid_tenant_id", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "bad tenant id with spaces",
			BindingKey:     "api:space:VTI",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("invalid tenant id: want Invalid, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("validation_invalid_binding_key", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-vbk-tenant",
			BindingKey:     "bad-binding",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("invalid binding key: want Invalid, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("validation_blank_question", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-bq-tenant",
			BindingKey:     "api:space:CBQ",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		_, err := core.Query(ctx, brainapi.QueryRequest{
			TenantID: "contract-bq-tenant",
			Question: "",
		})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("blank question: want Invalid, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("validation_missing_source_uri", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-msu-tenant",
			BindingKey:     "api:space:CMSU",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: "contract-msu-tenant",
			JobID:    "contract-msu-job",
			Source:   brainapi.SourceRef{}, // empty URI
		})
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("missing source URI: want Invalid, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	// ─── Admin read operations ────────────────────────────────────────────────

	t.Run("admin_list_tenants_includes_created", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		const tenantID brainapi.TenantID = "contract-admin-lt-tenant"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       tenantID,
			BindingKey:     "api:space:CADLT",
			OwnerPrincipal: brainapi.Principal{ID: "owner-admin-lt"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		resp, err := core.AdminListTenants(ctx, brainapi.AdminListTenantsRequest{})
		if err != nil {
			t.Fatalf("AdminListTenants: %v", err)
		}
		var found *brainapi.TenantInfo
		for i := range resp.Tenants {
			if resp.Tenants[i].TenantID == tenantID {
				found = &resp.Tenants[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("tenant %q not found in AdminListTenants: %+v", tenantID, resp.Tenants)
		}
		if found.State != brainapi.ProjectOn {
			t.Fatalf("state = %q, want on", found.State)
		}
		if found.OwnerID != "owner-admin-lt" {
			t.Fatalf("ownerID = %q, want owner-admin-lt", found.OwnerID)
		}
	})

	t.Run("admin_list_bindings_filtered_by_tenant", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		const tenantID brainapi.TenantID = "contract-admin-lb-tenant"
		const bindingKey brainapi.BindingKey = "api:space:CADLB"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       tenantID,
			BindingKey:     bindingKey,
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		resp, err := core.AdminListBindings(ctx, brainapi.AdminListBindingsRequest{TenantID: tenantID})
		if err != nil {
			t.Fatalf("AdminListBindings: %v", err)
		}
		if len(resp.Bindings) != 1 {
			t.Fatalf("expected 1 binding for tenant, got %d: %+v", len(resp.Bindings), resp.Bindings)
		}
		if resp.Bindings[0].BindingKey != bindingKey || resp.Bindings[0].TenantID != tenantID {
			t.Fatalf("binding mismatch: %+v", resp.Bindings[0])
		}
	})

	t.Run("admin_list_bindings_unfiltered_includes_created", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		const bindingKey brainapi.BindingKey = "api:space:CADLBU"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-admin-lbu-tenant",
			BindingKey:     bindingKey,
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		resp, err := core.AdminListBindings(ctx, brainapi.AdminListBindingsRequest{})
		if err != nil {
			t.Fatalf("AdminListBindings unfiltered: %v", err)
		}
		found := false
		for _, b := range resp.Bindings {
			if b.BindingKey == bindingKey {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("binding %q not found in unfiltered list: %+v", bindingKey, resp.Bindings)
		}
	})

	t.Run("admin_list_sources_metadata_only", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		const tenantID brainapi.TenantID = "contract-admin-ls-tenant"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       tenantID,
			BindingKey:     "api:space:CADLS",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: tenantID,
			JobID:    "contract-admin-ls-job",
			Source:   brainapi.SourceRef{URI: "file://admin-source.txt", Name: "admin-source.txt"},
			Metadata: map[string]string{"content": "admin source content"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		resp, err := core.AdminListSources(ctx, tenantID)
		if err != nil {
			t.Fatalf("AdminListSources: %v", err)
		}
		if len(resp.Sources) != 1 {
			t.Fatalf("expected 1 source, got %d", len(resp.Sources))
		}
		src := resp.Sources[0]
		if src.TenantID != tenantID {
			t.Fatalf("source TenantID = %q, want %q", src.TenantID, tenantID)
		}
		if src.Name != "admin-source.txt" {
			t.Fatalf("source Name = %q, want admin-source.txt", src.Name)
		}
		if src.URI == "" {
			t.Fatalf("source URI is empty")
		}
		if src.ID == "" {
			t.Fatalf("source ID is empty")
		}
	})

	t.Run("admin_list_sources_works_when_project_off", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		const tenantID brainapi.TenantID = "contract-admin-ls-off-tenant"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       tenantID,
			BindingKey:     "api:space:CADLSOFF",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: tenantID,
			JobID:    "contract-admin-ls-off-job",
			Source:   brainapi.SourceRef{URI: "file://off-source.txt", Name: "off-source.txt"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if err := core.SetProjectState(ctx, tenantID, brainapi.ProjectOff); err != nil {
			t.Fatalf("SetProjectState Off: %v", err)
		}
		// Admin read must bypass "project is off" check.
		resp, err := core.AdminListSources(ctx, tenantID)
		if err != nil {
			t.Fatalf("AdminListSources on off project: %v", err)
		}
		if len(resp.Sources) != 1 {
			t.Fatalf("expected 1 source, got %d", len(resp.Sources))
		}
	})

	t.Run("admin_list_sources_invalid_tenant", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		_, err := core.AdminListSources(ctx, "bad tenant id")
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("invalid tenant: want Invalid, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("admin_list_jobs_and_get_job", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		const tenantID brainapi.TenantID = "contract-admin-lj-tenant"
		const jobID brainapi.JobID = "contract-admin-lj-job"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       tenantID,
			BindingKey:     "api:space:CADLJ",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: tenantID,
			JobID:    jobID,
			Source:   brainapi.SourceRef{URI: "file://job-source.txt"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		listResp, err := core.AdminListJobs(ctx, tenantID)
		if err != nil {
			t.Fatalf("AdminListJobs: %v", err)
		}
		if len(listResp.Jobs) != 1 {
			t.Fatalf("expected 1 job, got %d", len(listResp.Jobs))
		}
		if listResp.Jobs[0].JobID != jobID || listResp.Jobs[0].TenantID != tenantID {
			t.Fatalf("job mismatch: %+v", listResp.Jobs[0])
		}
		snap, err := core.AdminGetJob(ctx, tenantID, jobID)
		if err != nil {
			t.Fatalf("AdminGetJob: %v", err)
		}
		if snap.JobID != jobID || snap.TenantID != tenantID {
			t.Fatalf("snapshot mismatch: %+v", snap)
		}
		if snap.Status != brainapi.JobRunning {
			t.Fatalf("status = %q, want running", snap.Status)
		}
	})

	t.Run("admin_get_job_not_found", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       "contract-admin-gj-nf-tenant",
			BindingKey:     "api:space:CADGJNF",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		_, err := core.AdminGetJob(ctx, "contract-admin-gj-nf-tenant", "nonexistent-job")
		if !brainapi.IsKind(err, brainapi.KindNotFound) {
			t.Fatalf("want NotFound, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("admin_list_jobs_works_when_project_off", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		const tenantID brainapi.TenantID = "contract-admin-lj-off-tenant"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       tenantID,
			BindingKey:     "api:space:CADLJOFF",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: tenantID,
			JobID:    "contract-admin-lj-off-job",
			Source:   brainapi.SourceRef{URI: "file://off-job-source.txt"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if err := core.SetProjectState(ctx, tenantID, brainapi.ProjectOff); err != nil {
			t.Fatalf("SetProjectState Off: %v", err)
		}
		listResp, err := core.AdminListJobs(ctx, tenantID)
		if err != nil {
			t.Fatalf("AdminListJobs on off project: %v", err)
		}
		if len(listResp.Jobs) != 1 {
			t.Fatalf("expected 1 job, got %d: %+v", len(listResp.Jobs), listResp.Jobs)
		}
	})

	t.Run("admin_list_jobs_invalid_tenant", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		_, err := core.AdminListJobs(ctx, "bad tenant id")
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("invalid tenant: want Invalid, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("admin_get_job_invalid_inputs", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		_, err := core.AdminGetJob(ctx, "bad tenant id", "job-1")
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("invalid tenant: want Invalid, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
		_, err = core.AdminGetJob(ctx, "valid-tenant", "bad job id")
		if !brainapi.IsKind(err, brainapi.KindInvalid) {
			t.Fatalf("invalid job id: want Invalid, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	// ─── Grounding tier (#12) ─────────────────────────────────────────────────
	//
	// Source.Tier is a base Core requirement (not gated behind
	// SharedKnowledgeCore): every source returned by an ordinary project-tenant
	// query must report TierProject.

	t.Run("source_tier_defaults_to_project", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		const tenantID brainapi.TenantID = "contract-tier-default-tenant"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID:       tenantID,
			BindingKey:     "api:space:CTIERDEF",
			OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: tenantID,
			JobID:    "contract-tier-default-job",
			Source:   brainapi.SourceRef{URI: "file://tier.txt", Name: "tier.txt"},
			Metadata: map[string]string{"content": "tier default project content alpha"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		resp, err := core.Query(ctx, brainapi.QueryRequest{TenantID: tenantID, Question: "tier alpha"})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !resp.GroundingAvailable || len(resp.Sources) == 0 {
			t.Fatalf("expected grounded sources, got %+v", resp)
		}
		for _, src := range resp.Sources {
			if src.Tier != brainapi.TierProject {
				t.Fatalf("source Tier = %q, want %q", src.Tier, brainapi.TierProject)
			}
		}
	})

	// ─── Shared knowledge tenant (#12, SharedKnowledgeCore) ──────────────────
	//
	// These tests exercise the optional brainapi.SharedKnowledgeCore extension.
	// A Core implementation that does not (yet) implement it — e.g. postgres
	// before its C2 follow-up — skips these sub-tests rather than failing, so
	// the base suite stays green while the shared-tenant contract is adopted
	// incrementally across backends.

	sharedCoreOrSkip := func(t *testing.T, core brainapi.Core) brainapi.SharedKnowledgeCore {
		t.Helper()
		sk, ok := core.(brainapi.SharedKnowledgeCore)
		if !ok {
			t.Skip("core does not implement brainapi.SharedKnowledgeCore; skipping shared-tenant contract tests")
		}
		return sk
	}

	adminPrincipal := brainapi.Principal{ID: "root", Roles: []string{"admin"}}

	t.Run("shared_tenant_query_includes_bound_content_with_shared_tier", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		sk := sharedCoreOrSkip(t, core)

		// originTenant owns the source that gets promoted; it is never queried.
		const originTenant brainapi.TenantID = "contract-shared-origin"
		// queryTenant has no matching content of its own; it only sees the
		// promoted content because sharedTenant is bound to it.
		const queryTenant brainapi.TenantID = "contract-shared-query"
		const sharedTenant brainapi.TenantID = "contract-shared-tenant"

		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID: originTenant, BindingKey: "api:space:CSHORIGIN", OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject origin: %v", err)
		}
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID: queryTenant, BindingKey: "api:space:CSHQUERY", OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject query: %v", err)
		}
		if err := sk.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{
			TenantID: sharedTenant, OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		// originTenant must be bound too: PromoteSource requires the
		// destination shared tenant to already be bound to the promoting
		// project tenant (see PromoteRequest doc comment).
		if err := sk.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{
			ProjectTenantID: originTenant, SharedTenantID: sharedTenant,
		}); err != nil {
			t.Fatalf("BindSharedTenant origin: %v", err)
		}
		if err := sk.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{
			ProjectTenantID: queryTenant, SharedTenantID: sharedTenant,
		}); err != nil {
			t.Fatalf("BindSharedTenant query: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: originTenant,
			JobID:    "contract-shared-origin-job",
			Source:   brainapi.SourceRef{URI: "file://shared-origin.txt", Name: "shared-origin.txt"},
			Metadata: map[string]string{"content": "promotable shared keyword zulu content"},
		}); err != nil {
			t.Fatalf("Ingest origin: %v", err)
		}
		originSources, err := core.AdminListSources(ctx, originTenant)
		if err != nil {
			t.Fatalf("AdminListSources origin: %v", err)
		}
		if len(originSources.Sources) != 1 {
			t.Fatalf("expected 1 origin source, got %d", len(originSources.Sources))
		}

		if err := sk.PromoteSource(ctx, brainapi.PromoteRequest{
			Admin:           adminPrincipal,
			ProjectTenantID: originTenant,
			SharedTenantID:  sharedTenant,
			SourceID:        originSources.Sources[0].ID,
		}); err != nil {
			t.Fatalf("PromoteSource: %v", err)
		}

		// queryTenant itself never ingested anything with "zulu", so any
		// grounding must come from the bound shared tenant.
		resp, err := core.Query(ctx, brainapi.QueryRequest{TenantID: queryTenant, Question: "shared keyword zulu"})
		if err != nil {
			t.Fatalf("Query queryTenant: %v", err)
		}
		if !resp.GroundingAvailable {
			t.Fatalf("expected grounding from bound shared tenant, got %+v", resp)
		}
		var sawShared bool
		for _, src := range resp.Sources {
			if src.TenantID == originTenant {
				t.Fatalf("query tenant unexpectedly saw origin project tenant's source directly: %+v", src)
			}
			if src.TenantID == sharedTenant {
				sawShared = true
				if src.Tier != brainapi.TierShared {
					t.Fatalf("shared source Tier = %q, want %q", src.Tier, brainapi.TierShared)
				}
			}
		}
		if !sawShared {
			t.Fatalf("expected a source from bound shared tenant %q, got %+v", sharedTenant, resp.Sources)
		}
	})

	t.Run("shared_tenant_unbound_not_in_scope", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		sk := sharedCoreOrSkip(t, core)

		const originTenant brainapi.TenantID = "contract-shared-unbound-origin"
		const unboundTenant brainapi.TenantID = "contract-shared-unbound-query"
		const sharedTenant brainapi.TenantID = "contract-shared-unbound-tenant"

		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID: originTenant, BindingKey: "api:space:CSHUBORIGIN", OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject origin: %v", err)
		}
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID: unboundTenant, BindingKey: "api:space:CSHUBQUERY", OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject unbound: %v", err)
		}
		if err := sk.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{
			TenantID: sharedTenant, OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		// originTenant must be bound so PromoteSource (which requires the
		// destination shared tenant to be bound to the promoting project
		// tenant) succeeds below. unboundTenant is deliberately never bound
		// to sharedTenant — that is exactly the relationship under test.
		if err := sk.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{
			ProjectTenantID: originTenant, SharedTenantID: sharedTenant,
		}); err != nil {
			t.Fatalf("BindSharedTenant origin: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: originTenant,
			JobID:    "contract-shared-unbound-origin-job",
			Source:   brainapi.SourceRef{URI: "file://unbound-origin.txt", Name: "unbound-origin.txt"},
			Metadata: map[string]string{"content": "unbound shared keyword yankee content"},
		}); err != nil {
			t.Fatalf("Ingest origin: %v", err)
		}
		originSources, err := core.AdminListSources(ctx, originTenant)
		if err != nil {
			t.Fatalf("AdminListSources origin: %v", err)
		}
		if err := sk.PromoteSource(ctx, brainapi.PromoteRequest{
			Admin:           adminPrincipal,
			ProjectTenantID: originTenant,
			SharedTenantID:  sharedTenant,
			SourceID:        originSources.Sources[0].ID,
		}); err != nil {
			t.Fatalf("PromoteSource: %v", err)
		}

		resp, err := core.Query(ctx, brainapi.QueryRequest{TenantID: unboundTenant, Question: "shared keyword yankee"})
		if err != nil {
			t.Fatalf("Query unboundTenant: %v", err)
		}
		if resp.GroundingAvailable {
			t.Fatalf("unbound project unexpectedly saw shared tenant content: %+v", resp)
		}
		for _, src := range resp.Sources {
			if src.TenantID == sharedTenant {
				t.Fatalf("unbound project query returned a source from an unbound shared tenant: %+v", src)
			}
		}
	})

	t.Run("shared_tenant_direct_ingest_rejected", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		sk := sharedCoreOrSkip(t, core)

		const sharedTenant brainapi.TenantID = "contract-shared-direct-ingest"
		if err := sk.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{
			TenantID: sharedTenant, OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: sharedTenant,
			JobID:    "contract-shared-direct-ingest-job",
			Source:   brainapi.SourceRef{URI: "file://direct.txt"},
		})
		if !brainapi.IsKind(err, brainapi.KindUnauthorized) {
			t.Fatalf("direct ingest into shared tenant: want Unauthorized, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("shared_tenant_promote_requires_admin", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		sk := sharedCoreOrSkip(t, core)

		const projectTenant brainapi.TenantID = "contract-shared-promote-noadmin-project"
		const sharedTenant brainapi.TenantID = "contract-shared-promote-noadmin-shared"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID: projectTenant, BindingKey: "api:space:CSHPNA", OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := sk.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{
			TenantID: sharedTenant, OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		if err := sk.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{
			ProjectTenantID: projectTenant, SharedTenantID: sharedTenant,
		}); err != nil {
			t.Fatalf("BindSharedTenant: %v", err)
		}
		if err := core.Ingest(ctx, brainapi.IngestRequest{
			TenantID: projectTenant,
			JobID:    "contract-shared-promote-noadmin-job",
			Source:   brainapi.SourceRef{URI: "file://noadmin.txt", Name: "noadmin.txt"},
		}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		sources, err := core.AdminListSources(ctx, projectTenant)
		if err != nil {
			t.Fatalf("AdminListSources: %v", err)
		}
		nonAdmin := brainapi.Principal{ID: "member-1", Roles: []string{"member"}}
		err = sk.PromoteSource(ctx, brainapi.PromoteRequest{
			Admin:           nonAdmin,
			ProjectTenantID: projectTenant,
			SharedTenantID:  sharedTenant,
			SourceID:        sources.Sources[0].ID,
		})
		if !brainapi.IsKind(err, brainapi.KindUnauthorized) {
			t.Fatalf("promote without admin: want Unauthorized, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})

	t.Run("shared_tenant_bindings_reflect_registered_binding", func(t *testing.T) {
		t.Parallel()
		core := newCore()
		sk := sharedCoreOrSkip(t, core)

		const projectTenant brainapi.TenantID = "contract-shared-bindings-project"
		const sharedTenant brainapi.TenantID = "contract-shared-bindings-shared"
		if err := core.CreateProject(ctx, brainapi.CreateProjectRequest{
			TenantID: projectTenant, BindingKey: "api:space:CSHBIND", OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		if err := sk.CreateSharedTenant(ctx, brainapi.CreateSharedTenantRequest{
			TenantID: sharedTenant, OwnerPrincipal: brainapi.Principal{ID: "owner"},
		}); err != nil {
			t.Fatalf("CreateSharedTenant: %v", err)
		}
		before, err := sk.SharedBindings(ctx, projectTenant)
		if err != nil {
			t.Fatalf("SharedBindings before bind: %v", err)
		}
		if len(before) != 0 {
			t.Fatalf("expected no bindings before BindSharedTenant, got %+v", before)
		}
		if err := sk.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{
			ProjectTenantID: projectTenant, SharedTenantID: sharedTenant,
		}); err != nil {
			t.Fatalf("BindSharedTenant: %v", err)
		}
		after, err := sk.SharedBindings(ctx, projectTenant)
		if err != nil {
			t.Fatalf("SharedBindings after bind: %v", err)
		}
		if len(after) != 1 || after[0] != sharedTenant {
			t.Fatalf("SharedBindings = %+v, want [%q]", after, sharedTenant)
		}
		// Duplicate binding must be rejected.
		err = sk.BindSharedTenant(ctx, brainapi.BindSharedTenantRequest{
			ProjectTenantID: projectTenant, SharedTenantID: sharedTenant,
		})
		if !brainapi.IsKind(err, brainapi.KindAlreadyExists) {
			t.Fatalf("duplicate binding: want AlreadyExists, got kind=%q err=%v", brainapi.KindOf(err), err)
		}
	})
}
