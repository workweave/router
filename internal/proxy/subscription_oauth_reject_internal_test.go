package proxy

import (
	"errors"
	"net/http"
	"testing"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
)

func TestAnthropicOAuthCredentialRejected(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "403 permission_error (org disabled OAuth) is rejected",
			err: &providers.UpstreamErrorResponse{
				Status: http.StatusForbidden,
				Body:   []byte(`{"type":"error","error":{"type":"permission_error","message":"OAuth authentication is currently not allowed for this organization."}}`),
			},
			want: true,
		},
		{
			name: "401 authentication_error (expired/invalid token) is rejected",
			err: &providers.UpstreamErrorResponse{
				Status: http.StatusUnauthorized,
				Body:   []byte(`{"type":"error","error":{"type":"authentication_error","message":"Invalid authentication credentials"}}`),
			},
			want: true,
		},
		{
			name: "403 with an unrelated error type stays terminal",
			err: &providers.UpstreamErrorResponse{
				Status: http.StatusForbidden,
				Body:   []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"content policy"}}`),
			},
			want: false,
		},
		{
			name: "429 rate-limit is not an OAuth rejection (handled by IsRetryable)",
			err: &providers.UpstreamErrorResponse{
				Status: http.StatusTooManyRequests,
				Body:   []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`),
			},
			want: false,
		},
		{
			name: "400 authentication_error (wrong status) stays terminal",
			err: &providers.UpstreamErrorResponse{
				Status: http.StatusBadRequest,
				Body:   []byte(`{"type":"error","error":{"type":"authentication_error"}}`),
			},
			want: false,
		},
		{
			name: "unparseable body stays terminal",
			err: &providers.UpstreamErrorResponse{
				Status: http.StatusForbidden,
				Body:   []byte(`not json`),
			},
			want: false,
		},
		{
			name: "non-upstream error stays terminal",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "nil is not a rejection",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, anthropicOAuthCredentialRejected(tc.err))
		})
	}
}
