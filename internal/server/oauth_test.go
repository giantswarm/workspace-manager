package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
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

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/identity"
)

// fakeIdP is a minimal OIDC issuer on https://localhost: discovery document
// and JWKS, enough for mcp-oauth's Dex provider to boot and for forwarded
// id_tokens to be validated against it — the shape of the platform's Dex.
type fakeIdP struct {
	issuer string
	key    *rsa.PrivateKey
	caFile string
	srv    *httptest.Server
}

const testKID = "test-key"

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	idp := &fakeIdP{key: key}
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
	mux.HandleFunc("/dex/keys", func(w http.ResponseWriter, _ *http.Request) {
		jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: testKID, Algorithm: string(jose.RS256), Use: "sig"}}}
		_ = json.NewEncoder(w).Encode(jwks)
	})
	// Dex's userinfo answers for any unexpired token Dex signed, whatever its
	// audience — the fallback mcp-oauth takes for a JWT that names no trusted
	// audience, and the reason such a caller is known but not SSO.
	mux.HandleFunc("/dex/userinfo", func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var std jwt.Claims
		extra := map[string]any{}
		if err := parsed.Claims(&key.PublicKey, &std, &extra); err != nil || std.Validate(jwt.Expected{Time: time.Now()}) != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		extra["sub"] = std.Subject
		_ = json.NewEncoder(w).Encode(extra)
	})

	// mcp-oauth rejects IP-literal issuers even with private IPs allowed; a
	// hostname that resolves to loopback (localhost, like the agentlab Dex) is
	// what AllowPrivateIP is for. httptest's certificate has no localhost SAN,
	// so mint one.
	srv := httptest.NewUnstartedServer(mux)
	cert := selfSignedLocalhost(t)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	idp.srv = srv
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

// idToken mints an id_token the fake IdP signed.
func (f *fakeIdP) idToken(t *testing.T, aud []string, exp time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithHeader("kid", testKID).WithType("JWT"))
	require.NoError(t, err)
	claims := jwt.Claims{Issuer: f.issuer, Subject: "sub-admin", Audience: jwt.Audience(aud),
		Expiry: jwt.NewNumericDate(exp), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute))}
	extra := map[string]any{"email": "admin@lab.local", "email_verified": true, "name": "Lab Admin", "groups": []string{"platform-admins"}}
	tok, err := jwt.Signed(signer).Claims(claims).Claims(extra).Serialize()
	require.NoError(t, err)
	return tok
}

func (f *fakeIdP) config() OAuthConfig {
	return OAuthConfig{
		BaseURL:            "http://localhost:8080",
		DexIssuerURL:       f.issuer,
		DexClientID:        "agent-platform",
		DexClientSecret:    "lab-only",
		DexCAFile:          f.caFile,
		DexAllowPrivateIP:  true,
		TrustedAudiences:   []string{"agent-platform"},
		SSOAllowPrivateIPs: true,
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

// TestForwardedIDTokenBecomesTheCaller is the platform path: muster forwards
// the Dex id_token for the platform client; workspace-manager validates it
// against Dex's JWKS and the request carries the caller and the caller's
// token.
func TestForwardedIDTokenBecomesTheCaller(t *testing.T) {
	idp := newFakeIdP(t)
	o, err := newOAuth(idp.config(), "/mcp", quiet())
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	var seen struct {
		id    *identity.Identity
		token string
		ok    bool
	}
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, seen.ok = identity.FromContext(r.Context())
		seen.token, _ = identity.TokenFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	call := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := call("")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no token, no MCP")
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer", "RFC 9728 challenge")

	forwarded := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute))
	rec = call(forwarded)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, seen.ok, "the caller reaches the handler")
	assert.Equal(t, "admin@lab.local", seen.id.Email)
	assert.Equal(t, "sub-admin", seen.id.Subject)
	assert.Equal(t, []string{"platform-admins"}, seen.id.Groups)
	assert.Equal(t, identity.SourceSSO, seen.id.Source)
	assert.Equal(t, forwarded, seen.token, "the forwarded id_token travels with the request")

	// A token minted for some other client (another aggregator, another app)
	// is not trusted, even though the same IdP signed it.
	assert.Equal(t, http.StatusUnauthorized, call(idp.idToken(t, []string{"someone-else"}, time.Now().Add(30*time.Minute))).Code)
	// Expired tokens are rejected.
	assert.Equal(t, http.StatusUnauthorized, call(idp.idToken(t, []string{"agent-platform"}, time.Now().Add(-time.Minute))).Code)
	// Garbage is rejected.
	assert.Equal(t, http.StatusUnauthorized, call("not-a-token").Code)
}

// TestUntrustedAudienceIsNamedInTheRefusal: a session that signed in through
// another Dex client forwards an id_token for that client next to the
// audience the MCPServer requires, never the platform client. With neither
// trusted, mcp-oauth falls back to Dex's userinfo endpoint (Dex answers for
// any token it signed), so the caller is known but not SSO and there is no
// Dex token to carry. The refusal names the token's audiences and the trusted
// ones, in the log and in the WWW-Authenticate description muster shows,
// without any token material. Trusting the required audience turns the same
// token into the caller.
func TestUntrustedAudienceIsNamedInTheRefusal(t *testing.T) {
	idp := newFakeIdP(t)
	var logs bytes.Buffer
	o, err := newOAuth(idp.config(), "/mcp", slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	h := o.protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a caller without a token to present must not reach the handler")
	}))
	portal := idp.idToken(t, []string{"portal-client", "kubernetes"}, time.Now().Add(30*time.Minute))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+portal)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	www := rec.Header().Get("WWW-Authenticate")
	assert.Contains(t, www, `Bearer error="invalid_token"`)
	assert.Contains(t, www, `error_description="token audience [portal-client kubernetes] matches none of the trusted audiences [agent-platform]`, www)
	assert.Contains(t, rec.Body.String(), "matches none of the trusted audiences [agent-platform]")
	assert.NotContains(t, www, portal, "no token material in the challenge")
	assert.Contains(t, logs.String(), "request refused")
	assert.Contains(t, logs.String(), `aud="[portal-client kubernetes]"`)
	assert.Contains(t, logs.String(), `trustedAudiences=[agent-platform]`)
	assert.Contains(t, logs.String(), "caller=admin@lab.local", "the caller is known — userinfo authenticated it")
	assert.NotContains(t, logs.String(), portal, "no token material in the log")

	// The generic refusal stays for a bearer that is not a JWT with a
	// readable audience — here an opaque token the fake IdP does not know,
	// which mcp-oauth already rejects.
	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer opaque-unknown")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.NotContains(t, rec.Header().Get("WWW-Authenticate"), "matches none of the trusted audiences")

	// Trust what the MCPServer requires (kubernetes here, dex-k8s-authenticator
	// on a Giant Swarm cluster) and the portal's token is the caller's.
	cfg := idp.config()
	cfg.TrustedAudiences = []string{"agent-platform", "kubernetes"}
	o2, err := newOAuth(cfg, "/mcp", quiet())
	require.NoError(t, err)
	t.Cleanup(func() { o2.shutdown(context.Background()) })
	var seen *identity.Identity
	var seenToken string
	h2 := o2.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = identity.FromContext(r.Context())
		seenToken, _ = identity.TokenFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+portal)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.NotNil(t, seen)
	assert.Equal(t, identity.SourceSSO, seen.Source)
	assert.Equal(t, "admin@lab.local", seen.Email)
	assert.Equal(t, portal, seenToken, "the id_token travels with the request")
}

func TestUnverifiedAudience(t *testing.T) {
	payload := func(claims string) string {
		return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".sig"
	}
	for name, tc := range map[string]struct {
		token string
		want  []string
		ok    bool
	}{
		"single audience":    {payload(`{"aud":"portal-client","sub":"x"}`), []string{"portal-client"}, true},
		"audience list":      {payload(`{"aud":["portal-client","kubernetes"]}`), []string{"portal-client", "kubernetes"}, true},
		"no audience":        {payload(`{"sub":"x"}`), nil, false},
		"empty audience":     {payload(`{"aud":""}`), nil, false},
		"not json":           {payload(`{"aud":`), nil, false},
		"opaque":             {"not-a-jwt", nil, false},
		"four segments":      {"a.b.c.d", nil, false},
		"payload not base64": {"a.!!!.c", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := unverifiedAudience(tc.token)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}

	aud, ok := untrustedAudience(payload(`{"aud":["portal-client","kubernetes"]}`), []string{"agent-platform"})
	assert.True(t, ok)
	assert.Equal(t, []string{"portal-client", "kubernetes"}, aud)
	_, ok = untrustedAudience(payload(`{"aud":["portal-client","kubernetes"]}`), []string{"agent-platform", "kubernetes"})
	assert.False(t, ok, "a token naming a trusted audience was refused for another reason")
	_, ok = untrustedAudience("opaque", []string{"agent-platform"})
	assert.False(t, ok)

	assert.Equal(t, `aud [a b] vs [c]`, headerQuotedString(`aud ["a" b] vs [\c]`))
}

// TestMCPInitialize is the server as muster meets it: probes and the OAuth
// metadata stay open, an MCP initialize without a token gets 401 with the
// RFC 9728 challenge, and one carrying the forwarded Dex token succeeds.
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

	initialize := func(bearer string) *http.Response {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"muster","version":"test"}}}`
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	resp := initialize("")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "no token, no MCP")
	assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer")

	resp = initialize(idp.idToken(t, []string{"agent-platform"}, time.Now().Add(time.Minute)))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"serverInfo":{"name":"workspace-manager","version":"test"}`)
	assert.NotContains(t, string(body), `"error"`)
}
