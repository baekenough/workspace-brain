//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/sangyi/workspace-brain/internal/core/coretest"
	pgcore "github.com/sangyi/workspace-brain/internal/core/postgres"
	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// startPostgres spins up a Postgres container and returns its DSN.
// It waits until Postgres has logged the "ready to accept connections" message
// twice (once for template creation, once for the actual DB).
func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("braintest"),
		tcpostgres.WithUsername("brain"),
		tcpostgres.WithPassword("brain"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("warn: terminate container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

// sharedCore creates exactly one Core connected to the container, runs
// migrations once, and returns a factory that hands back that same Core.
// The contract suite uses globally-unique tenant/binding IDs per sub-test so
// sharing one DB across sub-tests is safe.
func sharedCoreFactory(t *testing.T, dsn string) func() brainapi.Core {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := pgcore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(c.Close)

	return func() brainapi.Core { return c }
}

// TestContractSuite runs the brainapi.Core black-box contract suite against the
// Postgres implementation.
func TestContractSuite(t *testing.T) {
	dsn := startPostgres(t)
	coretest.RunContractSuite(t, sharedCoreFactory(t, dsn))
}

// TestRLSIsolation verifies that tenant B cannot observe tenant A's data even
// when the connection's GUC is set to tenant B (RLS enforcement at the DB level).
func TestRLSIsolation(t *testing.T) {
	ctx := context.Background()
	dsn := startPostgres(t)

	c, err := pgcore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer c.Close()

	// Provision tenant A with secret content.
	if err := c.CreateProject(ctx, brainapi.CreateProjectRequest{
		TenantID:       "rls-tenant-a",
		BindingKey:     "api:space:RLSA",
		OwnerPrincipal: brainapi.Principal{ID: "owner-a"},
	}); err != nil {
		t.Fatalf("CreateProject A: %v", err)
	}
	if err := c.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "rls-tenant-a",
		JobID:    "rls-job-a",
		Source:   brainapi.SourceRef{URI: "file://secret-a.txt", Name: "secret-a.txt"},
		Metadata: map[string]string{"content": "super secret data for tenant a"},
	}); err != nil {
		t.Fatalf("Ingest A: %v", err)
	}

	// Provision tenant B with no content.
	if err := c.CreateProject(ctx, brainapi.CreateProjectRequest{
		TenantID:       "rls-tenant-b",
		BindingKey:     "api:space:RLSB",
		OwnerPrincipal: brainapi.Principal{ID: "owner-b"},
	}); err != nil {
		t.Fatalf("CreateProject B: %v", err)
	}

	// Query from tenant B must return no grounding.
	resp, err := c.Query(ctx, brainapi.QueryRequest{
		TenantID: "rls-tenant-b",
		Question: "secret",
	})
	if err != nil {
		t.Fatalf("Query B: %v", err)
	}
	if resp.GroundingAvailable {
		t.Fatalf("RLS violation: tenant B query returned grounding from tenant A data: %+v", resp)
	}
	for _, src := range resp.Sources {
		if src.TenantID == "rls-tenant-a" {
			t.Fatalf("RLS violation: tenant B received source owned by tenant A: %+v", src)
		}
	}

	// Discover from tenant B must return zero results.
	discResp, err := c.Discover(ctx, brainapi.DiscoverRequest{TenantID: "rls-tenant-b"})
	if err != nil {
		t.Fatalf("Discover B: %v", err)
	}
	if len(discResp.Results) != 0 {
		t.Fatalf("RLS violation: tenant B Discover returned %d results, want 0: %+v", len(discResp.Results), discResp.Results)
	}

	// JobStatus from tenant B for tenant A's job must return NotFound (RLS hides the row).
	_, err = c.JobStatus(ctx, "rls-tenant-b", "rls-job-a")
	if !brainapi.IsKind(err, brainapi.KindNotFound) {
		t.Fatalf("RLS violation: tenant B JobStatus for tenant A job returned %v, want NotFound", err)
	}

	t.Logf("RLS isolation verified: tenant B cannot observe tenant A data")
}

// TestRLSCraftedQueryIsolation verifies that per-request GUC scoping does not
// bleed: a subsequent request for tenant B after a request for tenant A must
// not see tenant A's data.
func TestRLSCraftedQueryIsolation(t *testing.T) {
	ctx := context.Background()
	dsn := startPostgres(t)

	c, err := pgcore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	defer c.Close()

	// Provision tenant A with content.
	if err := c.CreateProject(ctx, brainapi.CreateProjectRequest{
		TenantID:       "rls-craft-a",
		BindingKey:     "api:space:RLSCA",
		OwnerPrincipal: brainapi.Principal{ID: "owner"},
	}); err != nil {
		t.Fatalf("CreateProject A: %v", err)
	}
	if err := c.Ingest(ctx, brainapi.IngestRequest{
		TenantID: "rls-craft-a",
		JobID:    "rls-craft-job-a",
		Source:   brainapi.SourceRef{URI: "file://craft-secret.txt", Name: "craft-secret.txt"},
		Metadata: map[string]string{"content": "crafted attack secret payload"},
	}); err != nil {
		t.Fatalf("Ingest A: %v", err)
	}

	// Provision tenant B with no content.
	if err := c.CreateProject(ctx, brainapi.CreateProjectRequest{
		TenantID:       "rls-craft-b",
		BindingKey:     "api:space:RLSCB",
		OwnerPrincipal: brainapi.Principal{ID: "owner"},
	}); err != nil {
		t.Fatalf("CreateProject B: %v", err)
	}

	// Query A (sets GUC to rls-craft-a on the connection used).
	_, err = c.Query(ctx, brainapi.QueryRequest{
		TenantID: "rls-craft-a",
		Question: "crafted attack secret",
	})
	if err != nil {
		t.Fatalf("Query A: %v", err)
	}

	// Immediately after, query as B — GUC must be reset to rls-craft-b.
	resp, err := c.Query(ctx, brainapi.QueryRequest{
		TenantID: "rls-craft-b",
		Question: "crafted attack secret",
	})
	if err != nil {
		t.Fatalf("Query B after A: %v", err)
	}
	if resp.GroundingAvailable {
		t.Fatalf("Context bleed: tenant B query after tenant A returned grounding: %+v", resp)
	}
	for _, src := range resp.Sources {
		if src.TenantID == "rls-craft-a" {
			t.Fatalf("Context bleed: tenant B received source from tenant A: %+v", src)
		}
	}
	t.Logf("Per-request GUC scoping verified: %s",
		fmt.Sprintf("B after A: GroundingAvailable=%v sources=%d", resp.GroundingAvailable, len(resp.Sources)))
}
