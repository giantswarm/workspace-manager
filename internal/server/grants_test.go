package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

// TestProviderAppGrants: muster's call to a registration with
// auth.providerApps carries the person's Dex ID token as the bearer and the
// token of each App the person granted in X-Muster-Grant-<instance>. Every
// token its provider accepts reaches the tools on the context, side by side;
// a refused one is left off, as not granted; no grant header reaches the
// tools; a call nobody authenticated asks no provider.
func TestProviderAppGrants(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := idp.config()
	github := &countingKind{Kind: personKind{"ghu_alice": "alice"}}
	gitlab := &countingKind{Kind: personKind{"glpat_alice": "alice"}}
	grants := &Grants{Apps: []provider.Instance{{Name: "github", Kind: github}, {Name: "gitlab", Kind: gitlab}}}

	_, err := New(Config{Addr: "127.0.0.1:0", Grants: grants}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.Error(t, err, "no grants without OAuth")
	_, err = New(Config{Addr: "127.0.0.1:0", OAuth: &cfg, Grants: &Grants{}}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.Error(t, err, "no grants without an App")

	srv, err := New(Config{Addr: "127.0.0.1:0", OAuth: &cfg, Grants: grants}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.NoError(t, err)
	t.Cleanup(func() { srv.oauth.shutdown(context.Background()) })

	type seen struct {
		id      *identity.Identity
		tokens  map[string]string
		headers []string
	}
	var got seen
	h := srv.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{tokens: map[string]string{}}
		got.id, _ = identity.FromContext(r.Context())
		for _, name := range []string{"github", "gitlab"} {
			if tok, ok := identity.ProviderToken(r.Context(), name); ok {
				got.tokens[name] = tok
			}
			if v := r.Header.Get(muster.GrantHeader(name)); v != "" {
				got.headers = append(got.headers, name)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	call := func(bearer string, grants map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		for app, tok := range grants {
			req.Header.Set(muster.GrantHeader(app), tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	dexToken := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute))

	rec := call(dexToken, map[string]string{"github": "ghu_alice", "gitlab": "glpat_alice"})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, map[string]string{"github": "ghu_alice", "gitlab": "glpat_alice"}, got.tokens, "both Apps' tokens, side by side")
	assert.Empty(t, got.headers, "no grant header reaches the tools")
	assert.Equal(t, "admin@lab.local", got.id.Email, "the caller is the Dex token's")

	rec = call(dexToken, map[string]string{"github": "ghu_alice"})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, map[string]string{"github": "ghu_alice"}, got.tokens, "an App not granted sends no header and carries no token")

	rec = call(dexToken, map[string]string{"github": "ghu_alice", "gitlab": "revoked"})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, map[string]string{"github": "ghu_alice"}, got.tokens, "a token its provider refuses is not granted")

	rec = call(dexToken, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "no App granted: the tools still answer, naming what to grant")
	assert.Empty(t, got.tokens)

	asked := github.asked + gitlab.asked
	rec = call("", map[string]string{"github": "ghu_other", "gitlab": "glpat_other"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no Dex token")
	assert.Equal(t, asked, github.asked+gitlab.asked, "no provider asked for a call nobody authenticated")

	rec = call(dexToken, map[string]string{"github": "ghu_alice"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, github.asked, "a verified token is trusted for the cache's time")
}
