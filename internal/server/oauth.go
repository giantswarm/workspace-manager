package server

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	oauth "github.com/giantswarm/mcp-oauth"
	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/giantswarm/mcp-oauth/providers/dex"
	"github.com/giantswarm/mcp-oauth/security"
	"github.com/giantswarm/mcp-oauth/storage/memory"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

// OAuthConfig makes workspace-manager an OAuth 2.1 resource server (mcp-oauth)
// in front of the MCP endpoint. Dex is the authority: muster forwards the
// session's Dex id_token byte-identical (MCPServer auth.forwardToken, unpinned
// like every manager), which is validated here against Dex's JWKS when its
// audience is one of TrustedAudiences. The caller's identity and token then
// travel with the request (identity package); every Kubernetes call is made
// with that token, so a request that yields none is refused (401).
type OAuthConfig struct {
	// BaseURL is the public URL of this server: the OAuth issuer identifier
	// of its own authorization-server metadata (https, or http on loopback).
	BaseURL string

	DexIssuerURL    string
	DexClientID     string
	DexClientSecret string
	// DexCAFile is a PEM CA bundle for a Dex with a private certificate;
	// verifies discovery, token and JWKS calls. Empty: system trust.
	DexCAFile string
	// DexAllowPrivateIP lets the issuer resolve to a private/loopback address
	// (an in-cluster Dex).
	DexAllowPrivateIP bool

	// TrustedAudiences are the OAuth client ids whose Dex id_tokens are
	// accepted as bearers (SSO token forwarding): the platform client and
	// the audiences muster requires, what the kube-apiserver trusts. Empty:
	// only tokens this server issued itself.
	TrustedAudiences []string
	// SSOAllowPrivateIPs lets the JWKS endpoint used for forwarded tokens
	// resolve to a private address (an in-cluster Dex).
	SSOAllowPrivateIPs bool
	// AllowPublicClientRegistration lets MCP clients register over DCR
	// without a token (labs only).
	AllowPublicClientRegistration bool
}

// Validate checks required fields.
func (c OAuthConfig) Validate() error {
	if c.BaseURL == "" {
		return fmt.Errorf("oauth: base URL is required")
	}
	if err := validateHTTPS(c.BaseURL); err != nil {
		return err
	}
	if c.DexIssuerURL == "" || c.DexClientID == "" || c.DexClientSecret == "" {
		return fmt.Errorf("oauth: dex issuer URL, client ID and client secret are required")
	}
	for _, a := range c.TrustedAudiences {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("oauth: trusted audiences must not be empty strings")
		}
	}
	return nil
}

type oauthRuntime struct {
	server  *oauth.Server
	handler *handler.Handler
	store   *memory.Store
	cfg     OAuthConfig
	mcpPath string
	log     *slog.Logger
}

func newOAuth(cfg OAuthConfig, mcpPath string, log *slog.Logger) (*oauthRuntime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	rootCAs, err := loadRootCAs(cfg.DexCAFile)
	if err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}
	provider, err := dex.NewProvider(&dex.Config{
		IssuerURL:      cfg.DexIssuerURL,
		ClientID:       cfg.DexClientID,
		ClientSecret:   cfg.DexClientSecret,
		RedirectURL:    strings.TrimSuffix(cfg.BaseURL, "/") + "/oauth/callback",
		AllowPrivateIP: cfg.DexAllowPrivateIP,
		RootCAs:        rootCAs,
		Logger:         log,
	})
	if err != nil {
		return nil, fmt.Errorf("oauth: dex provider: %w", err)
	}
	store := memory.New()
	serverCfg := &oauth.ServerConfig{
		Issuer:                        cfg.BaseURL,
		AllowRefreshTokenRotation:     true,
		AllowPublicClientRegistration: cfg.AllowPublicClientRegistration,
		MaxClientsPerIP:               10,
		TrustedAudiences:              cfg.TrustedAudiences,
		AllowPrivateIPJWKS:            cfg.SSOAllowPrivateIPs,
		JWKSRootCAs:                   rootCAs,
	}
	srv, err := oauth.NewServer(provider, store, store, store, serverCfg, log,
		oauth.WithAuditor(security.NewAuditor(log, true)))
	if err != nil {
		return nil, fmt.Errorf("oauth: server: %w", err)
	}
	log.Info("OAuth resource server enabled", "issuer", cfg.BaseURL, "dex", cfg.DexIssuerURL, "trustedAudiences", cfg.TrustedAudiences)
	return &oauthRuntime{server: srv, handler: handler.New(srv, log), store: store, cfg: cfg, mcpPath: mcpPath, log: log}, nil
}

// loadRootCAs is the system pool plus the PEM bundle at caFile; (nil, nil)
// without a file, selecting the system trust everywhere the pool is used.
func loadRootCAs(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile) // #nosec G304 -- operator-provided path
	if err != nil {
		return nil, fmt.Errorf("read CA file %s: %w", caFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no CA certificate in %s", caFile)
	}
	return pool, nil
}

func (o *oauthRuntime) register(mux *http.ServeMux) {
	o.handler.RegisterAuthorizationServerMetadataRoutes(mux)
	o.handler.RegisterProtectedResourceMetadataRoutes(mux, o.mcpPath)
	mux.HandleFunc("/oauth/authorize", o.handler.ServeAuthorization)
	mux.HandleFunc("/oauth/token", o.handler.ServeToken)
	mux.HandleFunc("/oauth/callback", o.handler.ServeCallback)
	mux.HandleFunc("/oauth/register", o.handler.ServeClientRegistration)
	mux.HandleFunc("/oauth/revoke", o.handler.ServeTokenRevocation)
	mux.HandleFunc("/oauth/introspect", o.handler.ServeTokenIntrospection)
}

// protect requires a valid bearer token (mcp-oauth ValidateToken: this
// server's own access tokens, or a forwarded Dex id_token whose audience is
// trusted) and then attaches the caller to the request.
func (o *oauthRuntime) protect(next http.Handler) http.Handler {
	return o.handler.ValidateToken(o.attachIdentity(next))
}

// attachIdentity translates the validated mcp-oauth user into the request's
// identity and resolves the caller's Dex token: a forwarded id_token is the
// bearer itself; for a token this server issued, Dex's id_token is looked up
// in the store. A request that yields no Dex token is refused: every
// Kubernetes call presents it, and there is nothing else to act with.
func (o *oauthRuntime) attachIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		info, ok := handler.UserInfoFromContext(ctx)
		if !ok || info == nil {
			// ValidateToken admits nothing without user info; defensive.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id := &identity.Identity{Subject: info.ID, Email: info.Email, Name: info.Name, Groups: info.Groups, Source: identity.SourceOAuth}
		if info.IsSSO() {
			id.Source = identity.SourceSSO
		}
		tok := o.callerToken(ctx, r, info)
		if tok == "" {
			o.refuse(w, r, id)
			return
		}
		ctx = identity.ContextWithToken(identity.ContextWith(ctx, id), tok)
		o.log.Debug("authenticated request", "caller", id.String(), "source", id.Source, "path", r.URL.Path)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// refuse answers a request that yields no Dex token with 401 and names the
// cause. The usual one: the bearer is a Dex id_token (a JWT) whose audience
// is not trusted. mcp-oauth then authenticated the caller through Dex's
// userinfo endpoint instead (Dex answers for any token it signed), which is
// why the caller is known but not SSO and no token is in the local store. The
// token's aud is read without verification, for the message only. muster
// shows the WWW-Authenticate description in its session hint.
func (o *oauthRuntime) refuse(w http.ResponseWriter, r *http.Request, id *identity.Identity) {
	reason := "no identity token to act with towards the Kubernetes API"
	attrs := []any{"caller", id.String(), "source", id.Source, "path", r.URL.Path}
	if aud, ok := untrustedAudience(bearerToken(r), o.cfg.TrustedAudiences); ok {
		reason = fmt.Sprintf("token audience %v matches none of the trusted audiences %v (--oauth-trusted-audiences)", aud, o.cfg.TrustedAudiences)
		attrs = append(attrs, "aud", aud, "trustedAudiences", o.cfg.TrustedAudiences)
	}
	o.log.Warn("request refused: "+reason, attrs...)
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="`+headerQuotedString(reason)+`"`)
	http.Error(w, "unauthorized: "+reason, http.StatusUnauthorized)
}

// untrustedAudience is the aud claim of bearer when bearer is a JWT that
// names none of trusted. false for opaque bearers, for JWTs without a
// readable aud, and for tokens that do name a trusted audience.
func untrustedAudience(bearer string, trusted []string) ([]string, bool) {
	aud, ok := unverifiedAudience(bearer)
	if !ok {
		return nil, false
	}
	for _, a := range aud {
		for _, t := range trusted {
			if a == t {
				return nil, false
			}
		}
	}
	return aud, true
}

// unverifiedAudience reads the aud claim (a string or a list) of a compact
// JWS without verifying anything about it.
func unverifiedAudience(token string) ([]string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || len(claims.Aud) == 0 {
		return nil, false
	}
	var one string
	if json.Unmarshal(claims.Aud, &one) == nil && one != "" {
		return []string{one}, true
	}
	var many []string
	if json.Unmarshal(claims.Aud, &many) == nil && len(many) > 0 {
		return many, true
	}
	return nil, false
}

// headerQuotedString makes s safe inside a WWW-Authenticate quoted-string
// (RFC 6750 §3: no double quote, no backslash).
func headerQuotedString(s string) string {
	return strings.NewReplacer(`"`, "", `\`, "").Replace(s)
}

func bearerToken(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func (o *oauthRuntime) callerToken(ctx context.Context, r *http.Request, info *providers.UserInfo) string {
	bearer := bearerToken(r)
	if bearer == "" {
		return ""
	}
	if info.IsSSO() {
		return bearer
	}
	tok, err := o.store.GetToken(ctx, bearer)
	if err != nil || tok == nil {
		return ""
	}
	// mcp-oauth keeps the provider's id_token as the oauth2 token extra.
	idToken, _ := tok.Extra("id_token").(string)
	return idToken
}

func (o *oauthRuntime) shutdown(ctx context.Context) {
	// Shutdown stops the stores it was given.
	if err := o.server.Shutdown(ctx); err != nil {
		o.log.Warn("oauth server shutdown", "error", err)
	}
}

// validateHTTPS enforces OAuth 2.1's HTTPS requirement, allowing plain HTTP
// only for loopback development.
func validateHTTPS(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("oauth: invalid base URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("oauth: base URL must use https (http is allowed for loopback only): %s", baseURL)
	default:
		return fmt.Errorf("oauth: base URL scheme must be http or https: %s", baseURL)
	}
}
