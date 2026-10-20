package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

// TestPinnedRegistration: muster's call to the registration pinned to a
// provider's App carries the person's App token as the bearer and the Dex ID
// token in X-Muster-Id-Token. A token the provider accepts reaches the tools
// with the caller from the Dex token and the App token on the context; a
// missing or refused one is a 401, which muster answers with its own sign-in
// link; a missing or foreign Dex token is refused as without the pin.
func TestPinnedRegistration(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := idp.config()
	kind := personKind{"ghu_alice": "alice"}
	pin := &Pin{Grants: muster.Grants{Instance: "github", Server: "workspace-manager"}, Kind: kind}

	_, err := New(Config{Addr: "127.0.0.1:0", Pin: pin}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.Error(t, err, "no pin without OAuth")

	srv, err := New(Config{Addr: "127.0.0.1:0", OAuth: &cfg, Pin: pin}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.NoError(t, err)
	t.Cleanup(func() { srv.oauth.shutdown(context.Background()) })

	var seen struct {
		id    *identity.Identity
		token string
		ok    bool
	}
	h := srv.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, _ = identity.FromContext(r.Context())
		seen.token, seen.ok = identity.ProviderToken(r.Context(), "github")
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(bearer, idToken string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if idToken != "" {
			req.Header.Set(ForwardedIdentityHeader, idToken)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	dexToken := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute))
	appToken := "ghu_alice"

	rec := call(appToken, dexToken)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, seen.ok, "the App token reaches the tools")
	assert.Equal(t, appToken, seen.token)
	assert.Equal(t, "admin@lab.local", seen.id.Email, "the caller is the Dex token's")

	rec = call("", dexToken)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no grant in muster")
	assert.Contains(t, rec.Body.String(), "core_auth_login server=workspace-manager")

	rec = call("revoked", dexToken)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a token the provider refuses")
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`)

	rec = call(appToken, "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no forwarded identity")
	assert.Contains(t, rec.Body.String(), ForwardedIdentityHeader)

	rec = call(appToken, idp.idToken(t, []string{"someone-else"}, time.Now().Add(30*time.Minute)))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a Dex token for another client")
}

// personKind is a provider that knows its people's tokens: token → account.
type personKind map[string]string

func (k personKind) List(context.Context, oauth2.TokenSource, string) ([]provider.Item, error) {
	return nil, nil
}

func (k personKind) Readable(context.Context, oauth2.TokenSource, string, []string) ([]provider.Item, error) {
	return nil, nil
}

func (k personKind) SyncCredential(context.Context, provider.Secrets, string) (oauth2.TokenSource, error) {
	return nil, nil
}

func (k personKind) Person(_ context.Context, cred oauth2.TokenSource) (string, error) {
	tok, err := cred.Token()
	if err != nil {
		return "", err
	}
	if who, ok := k[tok.AccessToken]; ok {
		return who, nil
	}
	return "", provider.ErrCredentialRefused
}

func (k personKind) Hosts() provider.Hosts { return provider.Hosts{} }

func (k personKind) SecretRefs() []provider.SecretRef { return nil }

// countingKind counts the provider's answers about the person.
type countingKind struct {
	provider.Kind
	asked int
}

func (k *countingKind) Person(ctx context.Context, cred oauth2.TokenSource) (string, error) {
	k.asked++
	return k.Kind.Person(ctx, cred)
}

// TestPinCachesAVerifiedToken: a token the provider accepted is trusted for
// the cache's time without asking again; a refused one is never cached.
func TestPinCachesAVerifiedToken(t *testing.T) {
	kind := personKind{"ghu_alice": "alice"}
	counting := &countingKind{Kind: kind}
	g, err := newPinGuard(Pin{Grants: muster.Grants{Instance: "github", Server: "workspace-manager"}, Kind: counting}, quiet())
	require.NoError(t, err)

	tok := "ghu_alice"
	require.NoError(t, g.verifier.verify(context.Background(), counting, tok))
	require.NoError(t, g.verifier.verify(context.Background(), counting, tok))
	assert.Equal(t, 1, counting.asked, "asked once within the cache's time")

	require.ErrorIs(t, g.verifier.verify(context.Background(), counting, "revoked"), provider.ErrCredentialRefused)
	require.ErrorIs(t, g.verifier.verify(context.Background(), counting, "revoked"), provider.ErrCredentialRefused)
	assert.Equal(t, 3, counting.asked, "a refused token is asked about every time")

	_, err = newPinGuard(Pin{Kind: kind}, quiet())
	assert.Error(t, err, "no instance, no pin")
}
