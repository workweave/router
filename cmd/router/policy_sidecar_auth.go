package main

import (
	"fmt"
	"strings"
	"time"

	"workweave/router/internal/policyclient"
)

const (
	policySidecarAuthNone          = "none"
	policySidecarAuthGoogleIDToken = "google-id-token"
)

func buildHMMPolicyClient(sidecarURL, authMode string, timeout time.Duration) (*policyclient.Client, error) {
	return buildHMMPolicyClientWithGoogleIDTokenFactory(
		sidecarURL,
		authMode,
		timeout,
		policyclient.NewGoogleIDToken,
	)
}

func buildHMMPolicyClientWithGoogleIDTokenFactory(
	sidecarURL, authMode string,
	timeout time.Duration,
	newGoogleIDTokenClient func(string, time.Duration) (*policyclient.Client, error),
) (*policyclient.Client, error) {
	switch strings.ToLower(strings.TrimSpace(authMode)) {
	case "", policySidecarAuthNone:
		return policyclient.New(sidecarURL, nil, timeout), nil
	case policySidecarAuthGoogleIDToken:
		return newGoogleIDTokenClient(sidecarURL, timeout)
	default:
		return nil, fmt.Errorf(
			"unsupported ROUTER_HMM_SIDECAR_AUTH %q (expected %q or %q)",
			authMode,
			policySidecarAuthNone,
			policySidecarAuthGoogleIDToken,
		)
	}
}
