package grant_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/internal/grant"
)

func ecPEM(t *testing.T, curve elliptic.Curve) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func rsaPEM(t *testing.T, bits int) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func TestParseKeys(t *testing.T) {
	current, next := ecPEM(t, elliptic.P256()), rsaPEM(t, 2048)
	keys, err := grant.ParseKeys(map[string][]byte{"current": current, "next": next, "sealing": make([]byte, 32)}, "current")
	require.NoError(t, err)
	assert.Equal(t, "ES256", keys.Signing.Algorithm)
	require.Len(t, keys.Public.Keys, 2, "the signing key and the next one, not the sealing bytes")
	for _, k := range keys.Public.Keys {
		assert.True(t, k.IsPublic(), "%s is published public", k.KeyID)
		assert.Equal(t, "sig", k.Use)
		assert.NotEmpty(t, k.KeyID)
	}
	assert.ElementsMatch(t, []string{"ES256", "RS256"}, []string{keys.Public.Keys[0].Algorithm, keys.Public.Keys[1].Algorithm})

	again, err := grant.ParseKeys(map[string][]byte{"next": next, "current": current}, "next")
	require.NoError(t, err)
	assert.Equal(t, "RS256", again.Signing.Algorithm, "switching the reference signs with the next key")
	assert.Equal(t, keys.Public.Keys[0].KeyID, again.Public.Keys[0].KeyID, "key ids are the keys' thumbprints, stable across reads")

	p384, err := grant.ParseKeys(map[string][]byte{"k": ecPEM(t, elliptic.P384())}, "k")
	require.NoError(t, err)
	assert.Equal(t, "ES384", p384.Signing.Algorithm)

	for name, tc := range map[string]struct {
		data map[string][]byte
		want string
	}{
		"missing":   {map[string][]byte{"other": current}, `no entry "k"`},
		"raw bytes": {map[string][]byte{"k": make([]byte, 32)}, "not a PEM private key"},
		"public":    {map[string][]byte{"k": pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1}})}, "not a private key"},
		"short RSA": {map[string][]byte{"k": rsaPEM(t, 1024)}, "at least 2048"},
		"P-224":     {map[string][]byte{"k": ecPEM(t, elliptic.P224())}, "want P-256 or P-384"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := grant.ParseKeys(tc.data, "k")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), "BEGIN", "no key material in an error")
		})
	}
}

// source is a KeySource on a fake Secret, with a clock the test moves.
func source(t *testing.T, data map[string][]byte, now *time.Time) (*grant.KeySource, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keys", Namespace: "wm"}, Data: data})
	return &grant.KeySource{Client: cs, Namespace: "wm", Secret: "keys", Key: "current", Now: func() time.Time { return *now }}, cs
}

func TestSignedGrantVerifiesAgainstTheJWKS(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	keys, _ := source(t, map[string][]byte{"current": ecPEM(t, elliptic.P256())}, &now)
	signer := &grant.Signer{Keys: keys, Issuer: "https://wm.example.com", Audience: "kagent", TTL: 3 * time.Minute, Now: func() time.Time { return now }}
	require.NoError(t, signer.Validate())
	g := grant.Grant{
		Workspace: grant.Workspace{Namespace: "kagent", Name: "otter-mcp"},
		Volume:    grant.Volume{Driver: "nfs.csi.k8s.io", Handle: "nfs-server#share#pvc-1"},
		Mounts:    []grant.Mount{{SubPath: grant.SessionDirectory}, {SubPath: grant.Mirrors, ReadOnly: true}},
		Changed:   []string{"acme/api"},
		Providers: []grant.Provider{{Hostname: "github.com", Audience: "github", Scheme: "Basic", Username: "x-access-token", Broker: "workspace-manager"}},
	}
	token, expiry, err := signer.Sign(context.Background(), "alice-sub", g)
	require.NoError(t, err)
	assert.Equal(t, now.Add(3*time.Minute), expiry, "expires as configured")

	srv := httptest.NewServer(grant.JWKS(keys, nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var set jose.JSONWebKeySet
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&set))
	require.Len(t, set.Keys, 1)
	assert.True(t, set.Keys[0].IsPublic())

	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	require.NoError(t, err)
	require.Len(t, parsed.Headers, 1)
	assert.Equal(t, set.Keys[0].KeyID, parsed.Headers[0].KeyID, "the grant names the published key")
	var claims grant.Claims
	require.NoError(t, parsed.Claims(set.Key(parsed.Headers[0].KeyID)[0].Key, &claims))
	require.NoError(t, claims.ValidateWithLeeway(jwt.Expected{Issuer: "https://wm.example.com", Subject: "alice-sub",
		AnyAudience: jwt.Audience{"kagent"}, Time: now.Add(time.Minute)}, 0))
	assert.Equal(t, g, claims.Grant)
	assert.NotEmpty(t, claims.ID)
	assert.ErrorIs(t, claims.ValidateWithLeeway(jwt.Expected{Time: now.Add(4 * time.Minute)}, 0), jwt.ErrExpired)

	// The payload is kagent's claim names and nothing else.
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(payload, &raw))
	assert.ElementsMatch(t, []string{"iss", "sub", "aud", "exp", "nbf", "iat", "jti", "workspace", "volume", "mounts", "changed", "providers"}, keysOf(raw))
	assert.Equal(t, []any{map[string]any{"sub_path": "sessions/${SESSION_ID}"}, map[string]any{"sub_path": "mirrors", "read_only": true}}, raw["mounts"])
}

func TestKeysRotateWithBothPublished(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	keys, cs := source(t, map[string][]byte{"current": ecPEM(t, elliptic.P256())}, &now)
	k, err := keys.Keys(context.Background())
	require.NoError(t, err)
	require.Len(t, k.Public.Keys, 1)

	secret, err := cs.CoreV1().Secrets("wm").Get(context.Background(), "keys", metav1.GetOptions{})
	require.NoError(t, err)
	secret.Data["next"] = ecPEM(t, elliptic.P256())
	_, err = cs.CoreV1().Secrets("wm").Update(context.Background(), secret, metav1.UpdateOptions{})
	require.NoError(t, err)

	k, err = keys.Keys(context.Background())
	require.NoError(t, err)
	assert.Len(t, k.Public.Keys, 1, "read again only after KeysTTL")
	now = now.Add(grant.KeysTTL)
	k, err = keys.Keys(context.Background())
	require.NoError(t, err)
	assert.Len(t, k.Public.Keys, 2, "the next key is published beside the signing key")
}

func TestJWKSUnavailableWithoutTheKey(t *testing.T) {
	now := time.Now()
	keys, _ := source(t, map[string][]byte{"other": ecPEM(t, elliptic.P256())}, &now)
	rec := httptest.NewRecorder()
	grant.JWKS(keys, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, grant.JWKSPath, nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.NotContains(t, rec.Body.String(), "current", "the Secret's layout stays in the log")
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
