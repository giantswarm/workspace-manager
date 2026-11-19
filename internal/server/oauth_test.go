package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	oauth "github.com/giantswarm/mcp-oauth"
	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/identity"
)

// fakeIdP is a minimal OIDC issuer on https://localhost serving the discovery
// document, enough for mcp-oauth's Dex provider to boot — the shape of the
// platform's Dex.
type fakeIdP struct {
	issuer string
	caFile string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/dex/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.issuer,
			"authorization_endpoint":                idp.issuer + "/auth",
			"token_endpoint":                        idp.issuer + "/token",
			"userinfo_endpoint":                     idp.issuer + "/userinfo",
			"jwks_uri":                              idp.issuer + "/keys",
			"response_types_supported":              []string{"code"},
			"scopes_supported":                      []string{"openid", "email", "profile", "groups", "offline_access"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		})
	})

	// A hostname that resolves to loopback (localhost, like the agentlab Dex);
	// httptest's certificate has no localhost SAN, so mint one.
	srv := httptest.NewUnstartedServer(mux)
	cert := selfSignedLocalhost(t)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	require.NoError(t, err)
	idp.issuer = "https://localhost:" + port + "/dex"

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	idp.caFile = caPath
	return idp
}

func selfSignedLocalhost(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (f *fakeIdP) config() OAuthConfig {
	return OAuthConfig{
		BaseURL:          "http://localhost:8080",
		DexIssuerURL:     f.issuer,
		DexClientID:      "agent-platform",
		DexClientSecret:  "lab-only",
		DexCAFile:        f.caFile,
		TrustedAudiences: []string{"agent-platform"},
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestOAuthConfigValidation(t *testing.T) {
	dexOK := OAuthConfig{BaseURL: "https://wm.example.com", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}
	require.Error(t, OAuthConfig{}.Validate())
	require.Error(t, OAuthConfig{BaseURL: "http://wm.example.com", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}.Validate(), "plain http only on loopback")
	require.NoError(t, OAuthConfig{BaseURL: "http://localhost:8080", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}.Validate())
	require.NoError(t, dexOK.Validate())
	require.Error(t, OAuthConfig{BaseURL: "https://wm.example.com"}.Validate(), "dex needs issuer + client")
	bad := dexOK
	bad.TrustedAudiences = []string{"agent-platform", " "}
	require.Error(t, bad.Validate(), "empty audience")
}

// validatedUser stands for what mcp-oauth leaves on the context once it has
// validated a forwarded Dex id_token for the platform client.
var validatedUser = &providers.UserInfo{
	ID: "sub-admin", Email: "admin@lab.local", EmailVerified: true, Name: "Lab Admin", Groups: []string{"platform-admins"},
	TokenSource: providers.TokenSourceSSO,
}

const validatedBearer = "dex-id-token" //nolint:gosec // G101: a placeholder the validation stub matches, not a credential

// afterValidation replaces mcp-oauth's ValidateToken: the bearer
// validatedBearer is a validated forwarded id_token, anything else is refused.
func afterValidation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearerToken(r) != validatedBearer {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(handler.ContextWithUserInfo(r.Context(), validatedUser)))
	})
}

// guardAfterValidation is Server.guard with afterValidation in place of
// mcp-oauth's token validation.
func guardAfterValidation(s *Server, next http.Handler) http.Handler {
	h := next
	if s.grants != nil {
		h = s.grants.protect(h)
	}
	h = afterValidation(s.oauth.attachIdentity(h))
	if s.pin != nil {
		h = s.pin.protect(h)
	}
	return h
}

// TestAttachIdentityForwardedIDToken is the platform path after validation:
// muster forwarded the Dex id_token for the platform client and the request
// carries the caller and the caller's token.
func TestAttachIdentityForwardedIDToken(t *testing.T) {
	o := &oauthRuntime{log: quiet()}
	var seen struct {
		id    *identity.Identity
		token string
		ok    bool
	}
	h := o.attachIdentity(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, seen.ok = identity.FromContext(r.Context())
		seen.token, _ = identity.TokenFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+validatedBearer)
	req = req.WithContext(handler.ContextWithUserInfo(req.Context(), validatedUser))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, seen.ok, "the caller reaches the handler")
	assert.Equal(t, "admin@lab.local", seen.id.Email)
	assert.True(t, seen.id.EmailVerified, "the IdP's email_verified travels, for membership by email")
	assert.Equal(t, "sub-admin", seen.id.Subject)
	assert.Equal(t, []string{"platform-admins"}, seen.id.Groups)
	assert.Equal(t, identity.SourceSSO, seen.id.Source)
	assert.Equal(t, validatedBearer, seen.token, "the forwarded id_token travels with the request")

	seen.ok = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no validated user, no caller")
	assert.False(t, seen.ok)
}

func TestHeaderQuotedString(t *testing.T) {
	assert.Equal(t, `aud [a b] vs [c]`, headerQuotedString(`aud ["a" b] vs [\c]`))
}

// TestMCPInitialize is the server as muster meets it: probes and the OAuth
// metadata stay open and an MCP initialize without a token gets 401 with the
// RFC 9728 challenge.
func TestMCPInitialize(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := idp.config()
	srv, err := New(Config{Addr: "127.0.0.1:0", OAuth: &cfg}, api.NewMCPServer(api.Config{}, "test"), quiet())
	require.NoError(t, err)
	t.Cleanup(func() { srv.oauth.shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path string) int {
		resp, err := http.Get(ts.URL + path) // #nosec G107 -- the test server's URL
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusOK, get("/healthz"))
	assert.Equal(t, http.StatusOK, get("/readyz"))
	assert.Equal(t, http.StatusOK, get("/.well-known/oauth-authorization-server"), "metadata stays public")
	assert.Equal(t, http.StatusOK, get("/.well-known/oauth-protected-resource/mcp"), "metadata stays public")

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"muster","version":"test"}}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "no token, no MCP")
	assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer")
}

func TestOAuthServerConfig(t *testing.T) {
	rootCAs := x509.NewCertPool()
	cfg := OAuthConfig{BaseURL: "https://wm.example.com", TrustedAudiences: []string{"agent-platform"}, AllowPublicClientRegistration: true}
	assert.Equal(t, &oauth.ServerConfig{
		Issuer:                        "https://wm.example.com",
		AllowRefreshTokenRotation:     true,
		AllowPublicClientRegistration: true,
		MaxClientsPerIP:               10,
		TrustedAudiences:              []string{"agent-platform"},
		JWKSRootCAs:                   rootCAs,
	}, oauthServerConfig(cfg, rootCAs))
}
