package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func ctxWithCreds(creds *Credentials) context.Context {
	return context.WithValue(context.Background(), CredentialsContextKey{}, creds)
}

func TestCredentialKeyParts_DeploymentKeyTurnIsEmpty(t *testing.T) {
	s := &Service{}
	prefix, suffix, src := s.credentialKeyParts(context.Background())
	assert.Empty(t, prefix, "a deployment-key turn has no per-request credential to record")
	assert.Empty(t, suffix)
	assert.Empty(t, src)

	prefixNil, suffixNil, srcNil := s.credentialKeyParts(ctxWithCreds(nil))
	assert.Empty(t, prefixNil)
	assert.Empty(t, suffixNil)
	assert.Empty(t, srcNil)
}

func TestCredentialKeyParts_RecordsPrefixSuffixAndSource(t *testing.T) {
	s := &Service{}
	ctx := ctxWithCreds(&Credentials{APIKey: []byte("sk-ant-oat-vinh-1234"), Source: credSourceSubscription})

	prefix, suffix, src := s.credentialKeyParts(ctx)
	assert.Equal(t, "sk-ant-o", prefix)
	assert.Equal(t, "1234", suffix)
	assert.Equal(t, credSourceSubscription, src)
}

func TestCredentialKeyParts_SharedTokenAcrossUsersMatches(t *testing.T) {
	s := &Service{}
	shared := []byte("sk-ant-oat-shared-account")

	prefixA, suffixA, _ := s.credentialKeyParts(ctxWithCreds(&Credentials{APIKey: shared, Source: credSourceSubscription}))
	prefixB, suffixB, _ := s.credentialKeyParts(ctxWithCreds(&Credentials{APIKey: shared, Source: credSourceSubscription}))
	assert.Equal(t, prefixA, prefixB, "a shared token must have the same safe prefix regardless of caller")
	assert.Equal(t, suffixA, suffixB, "a shared token must have the same safe suffix regardless of caller")

	prefixOther, suffixOther, _ := s.credentialKeyParts(ctxWithCreds(&Credentials{APIKey: []byte("sk-ant-oat-different-9999"), Source: credSourceSubscription}))
	assert.NotEqual(t, prefixA+suffixA, prefixOther+suffixOther, "distinct tokens should differ in their safe display parts")
}

func TestCredentialKeyParts_ShortKey(t *testing.T) {
	s := &Service{}
	prefix, suffix, src := s.credentialKeyParts(ctxWithCreds(&Credentials{APIKey: []byte("short-key"), Source: credSourceClient}))
	assert.Equal(t, "short-key", prefix)
	assert.Empty(t, suffix)
	assert.Equal(t, credSourceClient, src)
}
