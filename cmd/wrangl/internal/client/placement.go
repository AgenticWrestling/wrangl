package client

import (
	"context"
	"net/http"
	"net/url"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// SetRunner changes a runner's settings (its capacity).
func (c *Client) SetRunner(ctx context.Context, name string, req proto.RunnerPatch) (proto.RunnerView, error) {
	var out proto.RunnerView
	return out, c.json(ctx, http.MethodPatch, "/api/v1/runners/"+url.PathEscape(name), req, &out)
}

// Placement returns the global default resource needs.
func (c *Client) Placement(ctx context.Context) (proto.PlacementView, error) {
	var out proto.PlacementView
	return out, c.json(ctx, http.MethodGet, "/api/v1/placement", nil, &out)
}

// SetPlacement changes the global default resource needs.
func (c *Client) SetPlacement(ctx context.Context, req proto.PlacementPatch) (proto.PlacementView, error) {
	var out proto.PlacementView
	return out, c.json(ctx, http.MethodPatch, "/api/v1/placement", req, &out)
}
