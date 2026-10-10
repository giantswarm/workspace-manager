package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
	"github.com/giantswarm/workspace-manager/internal/signin"
)

const (
	kagentID     = "kagent"
	kagentSecret = "kagent-secret"
	dexToken     = "dex-token-alice"
	stored       = "gho_stored_access"
)

var secretRef = provider.SecretRef{Name: "kagent-client", Key: "secret"}

type subjects map[string]*identity.Identity

func (s subjects) Subject(_ context.Context, token string) (*identity.Identity, error) {
	if id, ok := s[token]; ok {
		return id, nil
	}
	return nil, errors.New("invalid token")
}

type tokens struct {
	access signin.Access
	err    error
	calls  []string
}

func (t *tokens) Access(_ context.Context, person, instance string) (signin.Access, error) {
	t.calls = append(t.calls, person+"/"+instance)
	return t.access, t.err
}

type fixture struct {
	h      *Handler
	tokens *tokens
	logs   *bytes.Buffer
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	f := &fixture{tokens: &tokens{access: signin.Access{Token: stored, Expiry: now.Add(time.Hour)}}, logs: &bytes.Buffer{}, now: now}
	h, err := New(Config{
		ClientID:     kagentID,
		ClientSecret: secretRef,
		Secrets:      providertest.Secrets{secretRef: []byte(kagentSecret)},
		Instances:    []provider.Instance{{Name: "github"}, {Name: "other"}},
		Tokens:       f.tokens,
		ConnectURL:   func(instance string) string { return "https://wm.example.com/connect/" + instance },
		Logger:       slog.New(slog.NewJSONHandler(f.logs, nil)),
		Now:          func() time.Time { return now },
	}, subjects{dexToken: {Subject: "alice-sub", Email: "alice@example.com"}})
	require.NoError(t, err)
	f.h = h
	return f
}

func exchangeForm(audience string) url.Values {
	return url.Values{
		"grant_type":         {GrantType},
		"subject_token":      {dexToken},
		"subject_token_type": {TokenTypeIDToken},
		"audience":           {audience},
	}
}

func (f *fixture) post(form url.Values, basic bool) *httptest.ResponseRecorder {
	if !basic {
		if !form.Has("client_id") {
			form.Set("client_id", kagentID)
		}
		if !form.Has("client_secret") {
			form.Set("client_secret", kagentSecret)
		}
	}
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic {
		req.SetBasicAuth(url.QueryEscape(kagentID), url.QueryEscape(kagentSecret))
	}
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	f.h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m), rec.Body.String())
	return m
}

func TestReleasesTheStoredAccessToken(t *testing.T) {
	for name, basic := range map[string]bool{"client_secret_basic": true, "client_secret_post": false} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			rec := f.post(exchangeForm("github"), basic)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			assert.Equal(t, map[string]any{
				"access_token":      stored,
				"issued_token_type": TokenTypeAccessToken,
				"token_type":        "Bearer",
				"expires_in":        float64(3600),
			}, decode(t, rec), "never a refresh token")
			assert.Equal(t, []string{"alice-sub/github"}, f.tokens.calls, "the person is the Dex subject")

			assert.Contains(t, f.logs.String(), `"msg":"token released"`)
			assert.Contains(t, f.logs.String(), `"person":"alice@example.com"`)
			assert.Contains(t, f.logs.String(), `"instance":"github"`)
			assert.Contains(t, f.logs.String(), `"client":"kagent"`)
			assert.NotContains(t, f.logs.String(), stored)
			assert.NotContains(t, f.logs.String(), dexToken)
			assert.NotContains(t, f.logs.String(), kagentSecret)
		})
	}
}

func TestRefusesEveryOtherClient(t *testing.T) {
	for name, form := range map[string]url.Values{
		"another client":   {"client_id": {"someone"}, "client_secret": {kagentSecret}},
		"wrong secret":     {"client_id": {kagentID}, "client_secret": {"guess"}},
		"no secret":        {"client_id": {kagentID}, "client_secret": {""}},
		"no client at all": {"client_id": {""}, "client_secret": {""}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			body := exchangeForm("github")
			for k, v := range form {
				body[k] = v
			}
			rec := f.post(body, false)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "invalid_client", decode(t, rec)["error"])
			assert.Empty(t, f.tokens.calls, "nothing is looked up for an unknown client")
			assert.NotContains(t, rec.Body.String(), stored)
		})
	}

	t.Run("basic with a wrong secret asks for Basic", func(t *testing.T) {
		f := newFixture(t)
		req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(exchangeForm("github").Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(kagentID, "guess")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, "invalid_client", decode(t, rec)["error"])
		assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Basic")
	})
}

func TestNoSignInPointsAtTheConnectPage(t *testing.T) {
	for name, err := range map[string]error{"not signed in": signin.ErrNotSignedIn, "refresh revoked": signin.ErrSignInExpired} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.tokens.err = err
			rec := f.post(exchangeForm("other"), false)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			m := decode(t, rec)
			assert.Equal(t, "invalid_target", m["error"])
			assert.Equal(t, "https://wm.example.com/connect/other", m["error_uri"])
		})
	}
}

func TestUnknownAudienceHasNoConnectPage(t *testing.T) {
	f := newFixture(t)
	rec := f.post(exchangeForm("gitlab"), false)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	m := decode(t, rec)
	assert.Equal(t, "invalid_target", m["error"])
	assert.NotContains(t, m, "error_uri")
	assert.Empty(t, f.tokens.calls)
}

func TestRefusesInvalidRequests(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(url.Values)
		code string
	}{
		"invalid subject token":   {func(v url.Values) { v.Set("subject_token", "forged") }, "invalid_request"},
		"no subject token":        {func(v url.Values) { v.Del("subject_token") }, "invalid_request"},
		"unknown subject type":    {func(v url.Values) { v.Set("subject_token_type", "urn:x:saml") }, "invalid_request"},
		"no audience":             {func(v url.Values) { v.Del("audience") }, "invalid_request"},
		"two audiences":           {func(v url.Values) { v.Add("audience", "other") }, "invalid_target"},
		"refresh token requested": {func(v url.Values) { v.Set("requested_token_type", "urn:ietf:params:oauth:token-type:refresh_token") }, "invalid_request"},
		"actor token":             {func(v url.Values) { v.Set("actor_token", "x") }, "invalid_request"},
		"another grant":           {func(v url.Values) { v.Set("grant_type", "client_credentials") }, "unsupported_grant_type"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			form := exchangeForm("github")
			tc.edit(form)
			rec := f.post(form, false)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code, decode(t, rec)["error"])
			assert.Empty(t, f.tokens.calls)
			assert.Contains(t, f.logs.String(), `"msg":"token exchange refused"`)
		})
	}
}

func TestRefusesTwoClientAuthenticationMethods(t *testing.T) {
	f := newFixture(t)
	form := exchangeForm("github")
	form.Set("client_secret", kagentSecret)
	rec := f.post(form, true)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "invalid_request", decode(t, rec)["error"])
}

func TestRefusesANonFormBody(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(`{"grant_type":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "invalid_request", decode(t, rec)["error"])
}

func TestOnlyPOST(t *testing.T) {
	f := newFixture(t)
	mux := http.NewServeMux()
	f.h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestStoreFailureIsAServerError(t *testing.T) {
	f := newFixture(t)
	f.tokens.err = errors.New("api server down")
	rec := f.post(exchangeForm("github"), false)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "server_error", decode(t, rec)["error"])
}

func TestUnreadableClientSecretIsAServerError(t *testing.T) {
	f := newFixture(t)
	f.h.cfg.Secrets = providertest.Secrets{}
	rec := f.post(exchangeForm("github"), false)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "server_error", decode(t, rec)["error"])
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	_, err := New(Config{}, subjects{})
	require.Error(t, err)
	_, err = New(Config{
		ClientID: kagentID, ClientSecret: secretRef, Secrets: providertest.Secrets{},
		Tokens: &tokens{}, ConnectURL: func(string) string { return "" },
	}, nil)
	require.Error(t, err, "no subject validation without OAuth")
}
