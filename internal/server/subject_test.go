package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/exchange"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
	"github.com/giantswarm/workspace-manager/internal/signin"
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

type storedAccess struct{ person string }

func (s *storedAccess) Access(_ context.Context, person, _ string) (signin.Access, error) {
	s.person = person
	return signin.Access{Token: "stored-access", Expiry: time.Now().Add(time.Hour)}, nil
}

// TestTokenExchangeRoute: with OAuth the listener serves POST /token, whose
// subject token the OAuth layer validates; a forged, expired or foreign one
// is refused, a Dex token for a trusted audience is exchanged.
func TestTokenExchangeRoute(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := idp.config()
	ref := provider.SecretRef{Name: "kagent-client", Key: "secret"}
	tokens := &storedAccess{}
	te := &exchange.Config{
		ClientID: "kagent", ClientSecret: ref,
		Secrets:    providertest.Secrets{ref: []byte("kagent-secret")},
		Instances:  []provider.Instance{{Name: "github"}},
		Tokens:     tokens,
		ConnectURL: func(i string) string { return "http://localhost:8080/connect/" + i },
		Logger:     quiet(),
	}

	_, err := New(Config{Addr: "127.0.0.1:0", TokenExchange: te}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.Error(t, err, "no token exchange without OAuth")

	srv, err := New(Config{Addr: "127.0.0.1:0", OAuth: &cfg, TokenExchange: te}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.NoError(t, err)
	t.Cleanup(func() { srv.oauth.shutdown(context.Background()) })

	post := func(subject string) (int, map[string]any) {
		form := url.Values{
			"grant_type": {exchange.GrantType}, "subject_token": {subject},
			"subject_token_type": {exchange.TokenTypeIDToken}, "audience": {"github"},
		}
		req := httptest.NewRequest(http.MethodPost, exchange.Path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("kagent", "kagent-secret")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
		return rec.Code, body
	}

	code, body := post(idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute)))
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "stored-access", body["access_token"])
	assert.Equal(t, "sub-admin", tokens.person, "the person is the Dex subject")

	forger := *idp
	forger.key, err = rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	for name, subject := range map[string]string{
		"forged":  forger.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute)),
		"expired": idp.idToken(t, []string{"agent-platform"}, time.Now().Add(-time.Minute)),
		"foreign": idp.idToken(t, []string{"someone-else"}, time.Now().Add(30*time.Minute)),
	} {
		code, body := post(subject)
		assert.Equal(t, http.StatusBadRequest, code, name)
		assert.Equal(t, "invalid_request", body["error"], name)
	}
}
