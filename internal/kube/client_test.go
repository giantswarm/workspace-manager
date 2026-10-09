package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

func testClients(t *testing.T) *Clients {
	t.Helper()
	c, err := fromRESTConfig(&rest.Config{ // #nosec G101 -- test fixture, not a credential
		Host:            "https://kubernetes.example.test:6443",
		BearerToken:     "service-account-token",
		BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		UserAgent:       "workspace-manager",
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	})
	require.NoError(t, err)
	return c
}

func TestForTokenDropsEveryServiceAccountCredential(t *testing.T) {
	c := testClients(t)
	user, err := c.ForToken("user-id-token")
	require.NoError(t, err)
	assert.Equal(t, "user-id-token", user.restCfg.BearerToken)
	assert.Empty(t, user.restCfg.BearerTokenFile, "the pod's token file must not shadow the caller's token")
	assert.Equal(t, c.restCfg.Host, user.restCfg.Host)
	assert.Equal(t, c.restCfg.TLSClientConfig, user.restCfg.TLSClientConfig)
	assert.Equal(t, "workspace-manager", user.restCfg.UserAgent)
	assert.NotSame(t, c.Typed(), user.Typed())

	_, err = c.ForToken("")
	assert.ErrorIs(t, err, ErrNoCallerToken)
}

func TestCallerProviderNeedsTheCallerToken(t *testing.T) {
	p := NewCallerProvider(testClients(t), nil)

	_, err := p.Client(context.Background())
	assert.ErrorIs(t, err, ErrNoCallerToken, "no token, no client: there is no other credential to fall back to")

	valid := jwt(t, time.Now().Add(time.Hour))
	ctx := identity.ContextWithToken(identity.ContextWith(context.Background(), &identity.Identity{Email: "admin@lab.local"}), valid)
	user, err := p.Client(ctx)
	require.NoError(t, err)
	assert.Equal(t, valid, user.(*Clients).restCfg.BearerToken, "the caller's token is what the apiserver sees")
	again, err := p.Client(ctx)
	require.NoError(t, err)
	assert.Same(t, user, again, "cached per token")

	other := identity.ContextWithToken(context.Background(), jwt(t, time.Now().Add(time.Hour), 7))
	second, err := p.Client(other)
	require.NoError(t, err)
	assert.NotSame(t, user, second, "another token, another client")

	expired := identity.ContextWithToken(context.Background(), jwt(t, time.Now().Add(-time.Minute)))
	stale, err := p.Client(expired)
	require.NoError(t, err, "an expired token is still presented: the apiserver rejects it, nothing else answers")
	again, err = p.Client(expired)
	require.NoError(t, err)
	assert.Same(t, stale, again)

	opaque := identity.ContextWithToken(context.Background(), "opaque")
	_, err = p.Client(opaque)
	require.NoError(t, err, "a token without exp is presented as long as the request lasts")
}

func TestCallerProviderEvictsExpiredEntriesWhenFull(t *testing.T) {
	p := NewCallerProvider(testClients(t), nil)
	for i := 0; i < maxCallerClients; i++ {
		_, err := p.Client(identity.ContextWithToken(context.Background(), jwt(t, time.Now().Add(-time.Hour), i)))
		require.NoError(t, err)
	}
	assert.Len(t, p.byToken, maxCallerClients)
	_, err := p.Client(identity.ContextWithToken(context.Background(), jwt(t, time.Now().Add(time.Hour), -1)))
	require.NoError(t, err)
	assert.Len(t, p.byToken, 1, "expired entries are evicted before a new one is cached")
}

func jwt(t *testing.T, exp time.Time, salt ...int) string {
	t.Helper()
	claims := map[string]any{"exp": exp.Unix()}
	if len(salt) > 0 {
		claims["jti"] = salt[0]
	}
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
