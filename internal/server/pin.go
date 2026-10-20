package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

// Pin makes the MCP endpoint the App-pinned registration muster connects
// with (MCPServer auth.authorizationServer pinned to the provider instance's
// App, grantScope subject, auth.forwardIdentity), the single-provider
// predecessor of Grants, which a registration may carry beside it: the bearer is the person's
// App token, muster's grant, verified with the provider and handed to the
// tools on the request's context; the person's Dex ID token arrives in
// ForwardedIdentityHeader and goes through the same validation a forwarded
// bearer takes without the pin.
type Pin struct {
	// Grants name the pinned provider instance and this server's MCPServer
	// in muster.
	Grants muster.Grants
	// Kind is the pinned instance's provider, which verifies the bearer.
	Kind provider.Kind
	// CacheTTL bounds how long a verified bearer is trusted without asking
	// the provider again (zero: DefaultPinCacheTTL).
	CacheTTL time.Duration
}

// ForwardedIdentityHeader is where muster puts the person's IdP ID token on
// every call to a registration with auth.forwardIdentity.
const ForwardedIdentityHeader = "X-Muster-Id-Token"

// DefaultPinCacheTTL is how long a verified bearer is trusted without asking
// the provider again: a revoked one must stop working soon.
const DefaultPinCacheTTL = 5 * time.Minute

// pinCacheMax bounds the verified-token cache.
const pinCacheMax = 10_000

// pinGuard verifies the App bearer and hands the forwarded ID token on as the
// bearer the Dex validation reads.
type pinGuard struct {
	pin      Pin
	verifier *tokenVerifier
	log      *slog.Logger
}

func newPinGuard(pin Pin, log *slog.Logger) (*pinGuard, error) {
	if pin.Grants.Instance == "" || pin.Grants.Server == "" || pin.Kind == nil {
		return nil, errors.New("muster pin: the provider instance, its provider and the MCPServer name are required")
	}
	v := newTokenVerifier(pin.CacheTTL)
	log.Info("muster pin enabled: the person's provider token is muster's grant", "provider", pin.Grants.Instance, "server", pin.Grants.Server, "cacheTTL", v.ttl)
	return &pinGuard{pin: pin, verifier: v, log: log}, nil
}

// protect admits a request whose bearer the provider accepts and that carries
// the forwarded ID token; next sees the ID token as the bearer and the
// person's provider token on the context. A refusal is a 401, which muster
// answers with its own sign-in link for the App.
func (g *pinGuard) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			g.refuse(w, "", "no bearer token: "+g.pin.Grants.SignIn(g.pin.Grants.Instance))
			return
		}
		idToken := strings.TrimSpace(r.Header.Get(ForwardedIdentityHeader))
		if idToken == "" {
			g.refuse(w, errInvalidToken, "no "+ForwardedIdentityHeader+" header: the workspace-manager checks your Organization from your platform identity, which muster forwards only with auth.forwardIdentity: true on the MCPServer")
			return
		}
		err := g.verifier.verify(r.Context(), g.pin.Kind, token)
		switch {
		case errors.Is(err, provider.ErrCredentialRefused):
			g.refuse(w, errInvalidToken, fmt.Sprintf("provider %s refused the bearer token: %s", g.pin.Grants.Instance, g.pin.Grants.SignIn(g.pin.Grants.Instance)))
			return
		case err != nil:
			g.log.Warn("bearer verification failed", "provider", g.pin.Grants.Instance, "error", err)
			http.Error(w, "could not verify the bearer token with provider "+g.pin.Grants.Instance+": "+err.Error(), http.StatusBadGateway)
			return
		}
		r = r.Clone(identity.ContextWithProviderToken(r.Context(), g.pin.Grants.Instance, token))
		r.Header.Set("Authorization", "Bearer "+idToken)
		r.Header.Del(ForwardedIdentityHeader)
		next.ServeHTTP(w, r)
	})
}

// refuse is the 401 with the RFC 6750 challenge and the reason as the body.
func (g *pinGuard) refuse(w http.ResponseWriter, code, description string) {
	challenge := `Bearer realm="workspace-manager"`
	if code != "" {
		challenge += fmt.Sprintf(`, error=%q, error_description=%q`, code, headerQuotedString(description))
	}
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, description, http.StatusUnauthorized)
}

// errInvalidToken is the RFC 6750 error code for a bearer the resource
// server does not accept.
const errInvalidToken = "invalid_token"
