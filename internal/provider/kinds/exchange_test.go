package kinds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/internal/exchange"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/fake"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
	"github.com/giantswarm/workspace-manager/internal/signin"
)

// subjectsByToken stands in for the OAuth layer's validation of the Dex
// token (internal/server tests it against a Dex).
type subjectsByToken map[string]string

func (s subjectsByToken) Subject(_ context.Context, token string) (*identity.Identity, error) {
	if sub, ok := s[token]; ok {
		return &identity.Identity{Subject: sub, Email: sub + "@example.com"}, nil
	}
	return nil, errors.New("invalid token")
}

// refreshServer is every instance's token endpoint: it counts the requests,
// rotating access and refresh tokens on each.
type refreshServer struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func newRefreshServer(t *testing.T) *refreshServer {
	rs := &refreshServer{}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := rs.calls.Add(1)
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("refreshed-%d", n),
			"refresh_token": fmt.Sprintf("refresh-%d", n),
			"token_type":    "bearer",
			"expires_in":    8 * 3600,
		})
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

type exchangeEnv struct {
	handler http.Handler
	store   *signin.KubeStore
	refresh *refreshServer
	logs    *bytes.Buffer
}

func newExchangeEnv(t *testing.T) *exchangeEnv {
	t.Helper()
	instances, err := testRegistry(fake.NewStore("t")).Load([]byte(twoProviders))
	require.NoError(t, err)
	rs := newRefreshServer(t)
	keyring, err := signin.NewKeyring(map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")}, "k1")
	require.NoError(t, err)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))
	store, err := signin.NewKubeStore(signin.Options{
		Client: k8sfake.NewClientset(), Namespace: "wm", Keyring: keyring, Identity: "test", Logger: log,
		OAuth2: signin.OAuth2ConfigFunc(func(_ context.Context, instance string) (*oauth2.Config, error) {
			return &oauth2.Config{ClientID: instance, ClientSecret: "s", Endpoint: oauth2.Endpoint{TokenURL: rs.srv.URL}}, nil
		}),
	})
	require.NoError(t, err)
	ref := provider.SecretRef{Name: "kagent-client", Key: "secret"}
	h, err := exchange.New(exchange.Config{
		ClientID: "kagent", ClientSecret: ref,
		Secrets:    providertest.Secrets{ref: []byte("kagent-secret")},
		Instances:  instances,
		Tokens:     store,
		ConnectURL: func(instance string) string { return "https://wm.example.com/connect/" + instance },
		Logger:     log,
	}, subjectsByToken{"dex-alice": "alice", "dex-bob": "bob"})
	require.NoError(t, err)
	mux := http.NewServeMux()
	h.Register(mux)
	return &exchangeEnv{handler: mux, store: store, refresh: rs, logs: logs}
}

func (e *exchangeEnv) exchange(t *testing.T, clientID, subject, audience string) (int, map[string]any) {
	t.Helper()
	form := url.Values{
		"grant_type":         {exchange.GrantType},
		"subject_token":      {subject},
		"subject_token_type": {exchange.TokenTypeIDToken},
		"audience":           {audience},
		"client_id":          {clientID},
		"client_secret":      {"kagent-secret"},
	}
	req := httptest.NewRequest(http.MethodPost, exchange.Path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	assert.NotContains(t, body, "refresh_token", "never a refresh token")
	assert.NotContains(t, rec.Body.String(), "refresh-", "no refresh token anywhere in the response")
	return rec.Code, body
}

// TestTokenExchangeContract is the endpoint's contract for every configured
// kind: the kagent client gets the person's access token for the GitHub
// instance and the fake one; another client gets invalid_client; a person
// without a sign-in gets invalid_target with the instance's connect page.
func TestTokenExchangeContract(t *testing.T) {
	ctx := context.Background()
	e := newExchangeEnv(t)
	for _, instance := range []string{"github", "other"} {
		t.Run(instance, func(t *testing.T) {
			require.NoError(t, e.store.Put(ctx, "alice", instance, &oauth2.Token{
				AccessToken: "access-" + instance, RefreshToken: "refresh-" + instance, Expiry: time.Now().Add(time.Hour),
			}))

			code, body := e.exchange(t, "kagent", "dex-alice", instance)
			require.Equal(t, http.StatusOK, code, body)
			assert.Equal(t, "access-"+instance, body["access_token"])
			assert.Equal(t, exchange.TokenTypeAccessToken, body["issued_token_type"])
			assert.Equal(t, "Bearer", body["token_type"])
			assert.InDelta(t, 3600, body["expires_in"], 5)

			code, body = e.exchange(t, "someone-else", "dex-alice", instance)
			assert.Equal(t, http.StatusUnauthorized, code)
			assert.Equal(t, "invalid_client", body["error"])

			code, body = e.exchange(t, "kagent", "dex-bob", instance)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "invalid_target", body["error"])
			assert.Equal(t, "https://wm.example.com/connect/"+instance, body["error_uri"])

			code, body = e.exchange(t, "kagent", "forged", instance)
			assert.Equal(t, http.StatusBadRequest, code)
			assert.Equal(t, "invalid_request", body["error"])
		})
	}
	assert.Zero(t, e.refresh.calls.Load(), "no provider request for valid tokens")
	assert.Equal(t, 2, strings.Count(e.logs.String(), `"msg":"token released"`), "one log line per release")
	assert.NotContains(t, e.logs.String(), "access-github")
	assert.NotContains(t, e.logs.String(), "access-other")
	assert.NotContains(t, e.logs.String(), "kagent-secret")
}

// TestTokenExchangeIsCheap: a hundred exchanges within the token's validity
// make no provider request; one inside the refresh margin refreshes once and
// releases the new access token, its refresh token staying in the store.
func TestTokenExchangeIsCheap(t *testing.T) {
	ctx := context.Background()
	e := newExchangeEnv(t)
	require.NoError(t, e.store.Put(ctx, "alice", "github", &oauth2.Token{
		AccessToken: "access-0", RefreshToken: "refresh-0", Expiry: time.Now().Add(time.Hour),
	}))
	for range 100 {
		code, body := e.exchange(t, "kagent", "dex-alice", "github")
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, "access-0", body["access_token"])
	}
	assert.Zero(t, e.refresh.calls.Load())
	assert.Equal(t, 100, strings.Count(e.logs.String(), `"msg":"token released"`))

	require.NoError(t, e.store.Put(ctx, "alice", "github", &oauth2.Token{
		AccessToken: "access-0", RefreshToken: "refresh-0", Expiry: time.Now().Add(time.Minute),
	}))
	code, body := e.exchange(t, "kagent", "dex-alice", "github")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "refreshed-1", body["access_token"], "refreshed ahead of expiry")
	assert.InDelta(t, 8*3600, body["expires_in"], 5)
	assert.EqualValues(t, 1, e.refresh.calls.Load())
}
