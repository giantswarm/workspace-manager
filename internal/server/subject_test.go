package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

// TestSubjectTokenOnTheBearersTerms: the token exchange's subject_token is
// accepted exactly when the MCP endpoint would accept it as the bearer: a
// Dex id_token for a trusted audience, signed by Dex, unexpired.
func TestSubjectTokenOnTheBearersTerms(t *testing.T) {
	idp := newFakeIdP(t)
	o, err := newOAuth(idp.config(), "/mcp", quiet())
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.Background()) })
	ctx := context.Background()

	id, err := o.Subject(ctx, idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute)))
	require.NoError(t, err)
	assert.Equal(t, "sub-admin", id.Subject)
	assert.Equal(t, "admin@lab.local", id.Email)
	assert.Equal(t, identity.SourceSSO, id.Source)

	// Forged: the same claims and key id, signed by a key Dex never had.
	forger := *idp
	forger.key, err = rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	forged := forger.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute))
	_, err = o.Subject(ctx, forged)
	assert.Error(t, err, "forged")

	_, err = o.Subject(ctx, idp.idToken(t, []string{"agent-platform"}, time.Now().Add(-time.Minute)))
	assert.Error(t, err, "expired")

	// Foreign: Dex signed it for another client; the userinfo fallback knows
	// the caller but yields no Dex token, so it is refused like the bearer.
	_, err = o.Subject(ctx, idp.idToken(t, []string{"someone-else"}, time.Now().Add(30*time.Minute)))
	assert.Error(t, err, "foreign audience")

	_, err = o.Subject(ctx, "not-a-token")
	assert.Error(t, err, "garbage")
	_, err = o.Subject(ctx, "")
	assert.Error(t, err, "empty")
}
