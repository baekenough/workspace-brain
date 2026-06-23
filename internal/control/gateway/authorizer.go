package gateway

import (
	"context"

	"github.com/sangyi/workspace-brain/pkg/brainapi"
)

// RoleAuthorizer is a small deterministic authorizer for the walking skeleton.
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
