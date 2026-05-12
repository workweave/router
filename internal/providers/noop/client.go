// Package noop is a placeholder providers.Client that always returns providers.ErrNotImplemented.
package noop

import (
	"context"
	"net/http"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
)

type Client struct{}

func NewClient() *Client {
	return &Client{}
}

func (c *Client) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	return providers.ErrNotImplemented
}

func (c *Client) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	return providers.ErrNotImplemented
}

var _ providers.Client = (*Client)(nil)
