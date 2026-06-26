package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// ── PolicyAuthorizer ─────────────────────────────────────────────────────────

func TestPolicyAuthorizerEmptyPrincipal(t *testing.T) {
	t.Parallel()
	a := NewPolicyAuthorizer(nil)
	err := a.Authorize(context.Background(), AuthorizationRequest{
		Action: brainapi.ActionQuery,
	})
	if !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("empty principal: kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestPolicyAuthorizerAdminRoleAllowsAll(t *testing.T) {
	t.Parallel()
	a := NewPolicyAuthorizer(nil)
	ctx := context.Background()
	admin := brainapi.Principal{ID: "U1", Roles: []string{"admin"}}
	for _, action := range []brainapi.Action{
		brainapi.ActionCreateProject,
		brainapi.ActionAdmin,
		brainapi.ActionQuery,
		brainapi.ActionDiscover,
		brainapi.ActionIngest,
		brainapi.ActionStatus,
	} {
		if err := a.Authorize(ctx, AuthorizationRequest{Principal: admin, Action: action}); err != nil {
			t.Fatalf("admin action %q: %v", action, err)
		}
	}
}

func TestPolicyAuthorizerCreateRequiresAdminOrAllowlist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// No allowlist: member cannot create.
	noAllowlist := NewPolicyAuthorizer(nil)
	member := brainapi.Principal{ID: "U1", Roles: []string{"member"}}
	if err := noAllowlist.Authorize(ctx, AuthorizationRequest{Principal: member, Action: brainapi.ActionCreateProject}); !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("member create without allowlist: kind=%q err=%v", brainapi.KindOf(err), err)
	}

	// Allowlist by raw ID: matched user can create.
	byID := NewPolicyAuthorizer(map[string]bool{"U1": true})
	if err := byID.Authorize(ctx, AuthorizationRequest{Principal: member, Action: brainapi.ActionCreateProject}); err != nil {
		t.Fatalf("allowlisted by ID: %v", err)
	}

	// Allowlist by full key: matched user can create.
	keyed := brainapi.Principal{Source: "slack", ID: "U2", Roles: []string{"member"}}
	byKey := NewPolicyAuthorizer(map[string]bool{"slack:U2": true})
	if err := byKey.Authorize(ctx, AuthorizationRequest{Principal: keyed, Action: brainapi.ActionCreateProject}); err != nil {
		t.Fatalf("allowlisted by key: %v", err)
	}

	// Allowlist does not match a different user.
	other := brainapi.Principal{ID: "U99", Roles: []string{"member"}}
	if err := byID.Authorize(ctx, AuthorizationRequest{Principal: other, Action: brainapi.ActionCreateProject}); !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("non-allowlisted member create: kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestPolicyAuthorizerAdminActionRequiresAdminRole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Even an allowlisted user cannot perform ActionAdmin without the admin role.
	a := NewPolicyAuthorizer(map[string]bool{"U1": true})
	member := brainapi.Principal{ID: "U1", Roles: []string{"member"}}
	if err := a.Authorize(ctx, AuthorizationRequest{Principal: member, Action: brainapi.ActionAdmin}); !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("allowlisted member admin action: kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestPolicyAuthorizerMemberRoleAllowsReadActions(t *testing.T) {
	t.Parallel()
	a := NewPolicyAuthorizer(nil)
	ctx := context.Background()
	member := brainapi.Principal{ID: "U1", Roles: []string{"member"}}
	for _, action := range []brainapi.Action{
		brainapi.ActionQuery,
		brainapi.ActionDiscover,
		brainapi.ActionIngest,
		brainapi.ActionStatus,
	} {
		if err := a.Authorize(ctx, AuthorizationRequest{Principal: member, Action: action}); err != nil {
			t.Fatalf("member action %q: %v", action, err)
		}
	}
}

func TestPolicyAuthorizerNoRoleDeniesReadActions(t *testing.T) {
	t.Parallel()
	a := NewPolicyAuthorizer(nil)
	ctx := context.Background()
	noRole := brainapi.Principal{ID: "U1"} // no roles
	if err := a.Authorize(ctx, AuthorizationRequest{Principal: noRole, Action: brainapi.ActionQuery}); !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("no-role query: kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

func TestNewPolicyAuthorizerClonesAllowlist(t *testing.T) {
	t.Parallel()
	original := map[string]bool{"U1": true}
	a := NewPolicyAuthorizer(original)
	// Mutating the original must not affect the authorizer.
	original["U99"] = true
	ctx := context.Background()
	other := brainapi.Principal{ID: "U99", Roles: []string{"member"}}
	if err := a.Authorize(ctx, AuthorizationRequest{Principal: other, Action: brainapi.ActionCreateProject}); !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("mutation of source map leaked into authorizer: kind=%q err=%v", brainapi.KindOf(err), err)
	}
}

// ── CachingAuthorizer ─────────────────────────────────────────────────────────

// counterAuthorizer counts delegate calls and delegates to an inner authorizer.
type counterAuthorizer struct {
	calls int
	inner Authorizer
}

func (c *counterAuthorizer) Authorize(ctx context.Context, req AuthorizationRequest) error {
	c.calls++
	return c.inner.Authorize(ctx, req)
}

func TestCachingAuthorizerSensitiveActionsAlwaysBypassCache(t *testing.T) {
	t.Parallel()
	delegate := &counterAuthorizer{inner: RoleAuthorizer{}}
	ca := NewCachingAuthorizer(delegate, time.Minute, fixedClock(time.Now()))

	ctx := context.Background()
	admin := brainapi.Principal{ID: "A", Roles: []string{"admin"}}
	req := AuthorizationRequest{Principal: admin, TenantID: "tenant-1"}

	for _, action := range []brainapi.Action{brainapi.ActionAdmin, brainapi.ActionCreateProject} {
		req.Action = action
		for i := 0; i < 3; i++ {
			if err := ca.Authorize(ctx, req); err != nil {
				t.Fatalf("sensitive action %q call %d: %v", action, i, err)
			}
		}
	}
	// 3 calls each for 2 sensitive actions = 6 live delegate calls; no caching.
	if delegate.calls != 6 {
		t.Fatalf("sensitive actions: expected 6 delegate calls, got %d", delegate.calls)
	}
}

func TestCachingAuthorizerCachesAllowDecision(t *testing.T) {
	t.Parallel()
	delegate := &counterAuthorizer{inner: RoleAuthorizer{}}
	mc := &mutableClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	ca := NewCachingAuthorizer(delegate, 5*time.Minute, mc.fn())

	ctx := context.Background()
	member := brainapi.Principal{ID: "U1", Roles: []string{"member"}}
	req := AuthorizationRequest{Principal: member, TenantID: "tenant-1", Action: brainapi.ActionQuery}

	// First call: miss — delegate is called.
	if err := ca.Authorize(ctx, req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if delegate.calls != 1 {
		t.Fatalf("expected 1 delegate call after miss, got %d", delegate.calls)
	}

	// Second call within TTL: hit — delegate is NOT called again.
	if err := ca.Authorize(ctx, req); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if delegate.calls != 1 {
		t.Fatalf("expected cache hit (still 1 delegate call), got %d", delegate.calls)
	}
}

func TestCachingAuthorizerCachesDenyDecision(t *testing.T) {
	t.Parallel()
	delegate := &counterAuthorizer{inner: RoleAuthorizer{}}
	mc := &mutableClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	ca := NewCachingAuthorizer(delegate, 5*time.Minute, mc.fn())

	ctx := context.Background()
	noRole := brainapi.Principal{ID: "U2"} // no roles — delegate will deny
	req := AuthorizationRequest{Principal: noRole, TenantID: "tenant-1", Action: brainapi.ActionQuery}

	// First call: miss — delegate is called, result is deny.
	if err := ca.Authorize(ctx, req); !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("first call should deny: kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if delegate.calls != 1 {
		t.Fatalf("expected 1 delegate call after miss, got %d", delegate.calls)
	}

	// Second call within TTL: cached deny — delegate is NOT called again.
	if err := ca.Authorize(ctx, req); !brainapi.IsKind(err, brainapi.KindUnauthorized) {
		t.Fatalf("cached deny: kind=%q err=%v", brainapi.KindOf(err), err)
	}
	if delegate.calls != 1 {
		t.Fatalf("expected cache hit (still 1 delegate call), got %d", delegate.calls)
	}
}

func TestCachingAuthorizerTTLExpiryTriggersLiveVerification(t *testing.T) {
	t.Parallel()
	delegate := &counterAuthorizer{inner: RoleAuthorizer{}}
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mc := &advancingClock{t: start}
	ca := NewCachingAuthorizer(delegate, 5*time.Minute, mc.now)

	ctx := context.Background()
	member := brainapi.Principal{ID: "U1", Roles: []string{"member"}}
	req := AuthorizationRequest{Principal: member, TenantID: "tenant-1", Action: brainapi.ActionQuery}

	// First call: miss.
	if err := ca.Authorize(ctx, req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if delegate.calls != 1 {
		t.Fatalf("expected 1 delegate call, got %d", delegate.calls)
	}

	// Advance clock past TTL (5 min + 1 sec).
	mc.t = start.Add(5*time.Minute + time.Second)

	// Next call: expired entry — must hit delegate again.
	if err := ca.Authorize(ctx, req); err != nil {
		t.Fatalf("post-expiry call: %v", err)
	}
	if delegate.calls != 2 {
		t.Fatalf("expected 2 delegate calls after expiry, got %d", delegate.calls)
	}
}

func TestCachingAuthorizerNilClockDefaultsToTimeNow(t *testing.T) {
	t.Parallel()
	// Passing nil clock must not panic; it defaults to time.Now.
	delegate := &counterAuthorizer{inner: RoleAuthorizer{}}
	ca := NewCachingAuthorizer(delegate, time.Minute, nil)

	ctx := context.Background()
	member := brainapi.Principal{ID: "U1", Roles: []string{"member"}}
	req := AuthorizationRequest{Principal: member, TenantID: "tenant-1", Action: brainapi.ActionQuery}
	if err := ca.Authorize(ctx, req); err != nil {
		t.Fatalf("nil clock: %v", err)
	}
}

func TestCachingAuthorizerFallsBackToBindingKeyWhenTenantIDEmpty(t *testing.T) {
	t.Parallel()
	delegate := &counterAuthorizer{inner: RoleAuthorizer{}}
	mc := &mutableClock{t: time.Now()}
	ca := NewCachingAuthorizer(delegate, time.Minute, mc.fn())

	ctx := context.Background()
	member := brainapi.Principal{ID: "U1", Roles: []string{"member"}}
	// TenantID is empty; cache key must use BindingKey.
	req := AuthorizationRequest{
		Principal:  member,
		BindingKey: "slack:channel:C1",
		Action:     brainapi.ActionQuery,
	}

	if err := ca.Authorize(ctx, req); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ca.Authorize(ctx, req); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if delegate.calls != 1 {
		t.Fatalf("expected cache hit on second call, got %d delegate calls", delegate.calls)
	}
}

// ── clock helpers ─────────────────────────────────────────────────────────────

// fixedClock returns a func that always returns t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// mutableClock is a thin wrapper so tests can swap out the time value while
// keeping the closure the same function pointer.
type mutableClock struct {
	t time.Time
}

func (mc *mutableClock) fn() func() time.Time {
	return func() time.Time { return mc.t }
}

// advancingClock exposes its field directly for test mutation.
type advancingClock struct {
	t time.Time
}

func (ac *advancingClock) now() time.Time { return ac.t }
