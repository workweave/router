package main

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/policyclient"
)

func TestBuildHMMPolicyClientPreservesUnauthenticatedLocalMode(t *testing.T) {
	client, err := buildHMMPolicyClient("http://localhost:8093", "none", time.Second)

	require.NoError(t, err)
	assert.NotNil(t, client)
}

func TestBuildHMMPolicyClientRejectsUnknownAuthMode(t *testing.T) {
	client, err := buildHMMPolicyClient("https://sidecar.internal", "api-key", time.Second)

	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "unsupported ROUTER_HMM_SIDECAR_AUTH")
}

func TestBuildHMMPolicyClientFailsClosedWhenGoogleCredentialsCannotBuild(t *testing.T) {
	wantErr := errors.New("ADC unavailable")
	client, err := buildHMMPolicyClientWithGoogleIDTokenFactory(
		"https://sidecar.internal",
		"google-id-token",
		time.Second,
		func(string, time.Duration) (*policyclient.Client, error) { return nil, wantErr },
	)

	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, client)
}
