package main

import (
	"fmt"
	"net/http"
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
	return buildPolicyClientWithGoogleIDTokenFactory(
		sidecarURL,
		authMode,
		timeout,
		nil,
		"ROUTER_HMM_SIDECAR_AUTH",
		newGoogleIDTokenClient,
	)
}

func buildConfiguredPolicyClient(
	sidecarURL, authMode string,
	timeout time.Duration,
	httpClient *http.Client,
) (*policyclient.Client, error) {
	return buildPolicyClientWithGoogleIDTokenFactory(
		sidecarURL,
		authMode,
		timeout,
		httpClient,
		"ROUTER_POLICY_SIDECAR_AUTH",
		policyclient.NewGoogleIDToken,
	)
}

func buildConfiguredPolicyClientWithGoogleIDTokenFactory(
	sidecarURL, authMode string,
	timeout time.Duration,
	httpClient *http.Client,
	newGoogleIDTokenClient func(string, time.Duration) (*policyclient.Client, error),
) (*policyclient.Client, error) {
	return buildPolicyClientWithGoogleIDTokenFactory(
		sidecarURL,
		authMode,
		timeout,
		httpClient,
		"ROUTER_POLICY_SIDECAR_AUTH",
		newGoogleIDTokenClient,
	)
}

func buildPolicyClientWithGoogleIDTokenFactory(
	sidecarURL, authMode string,
	timeout time.Duration,
	httpClient *http.Client,
	authSetting string,
	newGoogleIDTokenClient func(string, time.Duration) (*policyclient.Client, error),
) (*policyclient.Client, error) {
	switch strings.ToLower(strings.TrimSpace(authMode)) {
	case "", policySidecarAuthNone:
		return policyclient.New(sidecarURL, httpClient, timeout), nil
	case policySidecarAuthGoogleIDToken:
		return newGoogleIDTokenClient(sidecarURL, timeout)
	default:
		return nil, fmt.Errorf(
			"unsupported %s %q (expected %q or %q)",
			authSetting,
			authMode,
			policySidecarAuthNone,
			policySidecarAuthGoogleIDToken,
		)
	}
}
