// Package noop is a placeholder providers.Client that always returns
// providers.ErrNotImplemented. Useful as a default in dev/tests before real
// adapters land.
package noop

import (
	"context"
	"net/http"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
)

// ErrNotImplemented is re-exported for callers that historically referenced
// it from this package. New code should use providers.ErrNotImplemented.
var ErrNotImplemented = providers.ErrNotImplemented

type Client struct{}

func NewClient() *Client {
	return &Client{}
}

func (c *Client) Complete(ctx context.Context, req providers.Request) (providers.Response, error) {
	return providers.Response{}, providers.ErrNotImplemented
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	return providers.ErrNotImplemented
}

func (c *Client) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	return providers.ErrNotImplemented
}

var _ providers.Client = (*Client)(nil)
