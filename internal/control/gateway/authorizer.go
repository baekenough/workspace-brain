package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// RoleAuthorizer is a small deterministic authorizer for the local runtime.
type RoleAuthorizer struct{}

// Authorize allows admins to do everything and members to read/write existing tenants.
func (RoleAuthorizer) Authorize(ctx context.Context, req AuthorizationRequest) error {
	_ = ctx
	if req.Principal.ID == "" {
		return brainapi.E(brainapi.KindUnauthorized, "authorize", "principal is required", nil)
	}
	if req.Principal.HasRole("admin") {
		return nil
	}
	if req.Action == brainapi.ActionCreateProject || req.Action == brainapi.ActionAdmin {
		return brainapi.E(brainapi.KindUnauthorized, "authorize", "admin role is required", nil)
	}
	if req.Principal.HasRole("member") {
		return nil
	}
	return brainapi.E(brainapi.KindUnauthorized, "authorize", "member role is required", nil)
}

// PolicyAuthorizer implements the Authorizer seam with a configurable
// create-permission policy.
//
// Default: only principals with the "admin" role may perform ActionCreateProject
// or ActionAdmin.
//
// Optional: an allowlist of principal identifiers (matched against
// brainapi.Principal.Key and brainapi.Principal.ID) grants create-project
// permission to specific users without requiring the admin role. The allowlist
// is populated from config via Config.CreateAllowlistSet(), mirroring the
// AdminUserSet pattern.
type PolicyAuthorizer struct {
	// createAllowlist is an optional set of principal keys or IDs that may
	// perform ActionCreateProject in addition to admins.
	createAllowlist map[string]bool
}

// NewPolicyAuthorizer returns a PolicyAuthorizer with the given create allowlist.
// Pass nil or an empty map to enforce the default admin-only create policy.
func NewPolicyAuthorizer(createAllowlist map[string]bool) PolicyAuthorizer {
	clone := make(map[string]bool, len(createAllowlist))
	for k, v := range createAllowlist {
		clone[k] = v
	}
	return PolicyAuthorizer{createAllowlist: clone}
}

// Authorize implements Authorizer.
//
//   - Admin role: allowed for all actions.
//   - ActionCreateProject: allowed for admins and allowlisted principals.
//   - ActionAdmin: allowed for admins only.
//   - All other actions: allowed for principals with the "member" role.
func (a PolicyAuthorizer) Authorize(ctx context.Context, req AuthorizationRequest) error {
	_ = ctx
	if req.Principal.ID == "" {
		return brainapi.E(brainapi.KindUnauthorized, "authorize", "principal is required", nil)
	}
	if req.Principal.HasRole("admin") {
		return nil
	}
	if req.Action == brainapi.ActionCreateProject {
		if a.isAllowlisted(req.Principal) {
			return nil
		}
		return brainapi.E(brainapi.KindUnauthorized, "authorize", "admin role or create allowlist required", nil)
	}
	if req.Action == brainapi.ActionAdmin {
		return brainapi.E(brainapi.KindUnauthorized, "authorize", "admin role is required", nil)
	}
	if req.Principal.HasRole("member") {
		return nil
	}
	return brainapi.E(brainapi.KindUnauthorized, "authorize", "member role is required", nil)
}

// isAllowlisted reports whether p appears in the create allowlist.
// It matches against both the full principal key (source:id) and the raw id
// so the allowlist can be populated with either format.
func (a PolicyAuthorizer) isAllowlisted(p brainapi.Principal) bool {
	return a.createAllowlist[p.Key()] || a.createAllowlist[p.ID]
}

// isSensitiveAction reports whether the action must always be re-verified live,
// bypassing the membership cache.
func isSensitiveAction(action brainapi.Action) bool {
	return action == brainapi.ActionAdmin || action == brainapi.ActionCreateProject
}

// authCacheKey uniquely identifies a cached authorization decision.
type authCacheKey struct {
	principalKey string
	tenantScope  string // resolved tenantID, or bindingKey when tenantID is empty
	action       brainapi.Action
}

// authCacheEntry holds a cached authorization decision and when it was stored.
type authCacheEntry struct {
	allowed  bool
	cachedAt time.Time
}

// CachingAuthorizer wraps a delegate Authorizer with a TTL membership cache.
//
// Non-sensitive actions (query, discover, ingest, status) may use a cached
// decision, avoiding repeated calls to the delegate for the same
// (principal, tenant, action) triple. The cache is keyed on the resolved
// TenantID (or BindingKey when TenantID is absent), so distinct tenants never
// share an entry.
//
// Sensitive actions (ActionAdmin and ActionCreateProject) bypass the cache
// entirely and always call the delegate live.
//
// The now clock seam controls TTL expiry; inject a controllable func in tests
// rather than relying on real time.Sleep.
type CachingAuthorizer struct {
	delegate Authorizer
	ttl      time.Duration
	now      func() time.Time
	mu       sync.RWMutex
	cache    map[authCacheKey]*authCacheEntry
}

// NewCachingAuthorizer wraps delegate with a TTL membership cache.
//   - ttl is the duration for which cached decisions are considered fresh.
//   - now is the injectable clock; if nil, time.Now is used.
func NewCachingAuthorizer(delegate Authorizer, ttl time.Duration, now func() time.Time) *CachingAuthorizer {
	if now == nil {
		now = time.Now
	}
	return &CachingAuthorizer{
		delegate: delegate,
		ttl:      ttl,
		now:      now,
		cache:    make(map[authCacheKey]*authCacheEntry),
	}
}

// Authorize implements Authorizer.
//
// Sensitive actions (ActionAdmin, ActionCreateProject) always call the delegate
// live so membership changes are reflected in real time.
//
// For all other actions the decision is looked up in the in-memory cache. On a
// cache miss or a stale entry (older than ttl), the delegate is called and the
// result is stored for future requests.
func (c *CachingAuthorizer) Authorize(ctx context.Context, req AuthorizationRequest) error {
	// Sensitive actions always bypass the cache.
	if isSensitiveAction(req.Action) {
		return c.delegate.Authorize(ctx, req)
	}

	key := authCacheKey{
		principalKey: req.Principal.Key(),
		tenantScope:  string(req.TenantID),
		action:       req.Action,
	}
	if key.tenantScope == "" {
		key.tenantScope = string(req.BindingKey)
	}

	// Fast path: check cache under read lock.
	c.mu.RLock()
	entry, ok := c.cache[key]
	c.mu.RUnlock()

	if ok && c.now().Sub(entry.cachedAt) < c.ttl {
		if entry.allowed {
			return nil
		}
		return brainapi.E(brainapi.KindUnauthorized, "authorize", "cached: access denied", nil)
	}

	// Cache miss or expired: call the delegate and cache the result.
	err := c.delegate.Authorize(ctx, req)
	allowed := err == nil

	c.mu.Lock()
	c.cache[key] = &authCacheEntry{allowed: allowed, cachedAt: c.now()}
	c.mu.Unlock()

	return err
}
