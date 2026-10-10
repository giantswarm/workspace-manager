// Package connecttest fakes the parties of a provider sign-in for tests: an
// OAuth 2.0 authorization server that enforces PKCE and serves both the
// GitHub App paths and the RFC 7009 ones, a Dex that signs a test browser in
// as whichever person it carries, and that browser.
package connecttest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/giantswarm/mcp-oauth/providers"
	"golang.org/x/oauth2"
)

// Revocation is one revocation the authorization server accepted.
type Revocation struct {
	// Method is "grant" (GitHub's DELETE …/grant) or "rfc7009".
	Method string
	Token  string
}

// AuthServer is a fake authorization server for one OAuth client. It
// auto-approves every authorization request, issues single-use codes bound to
// the PKCE challenge and redirect URI, and refuses a token request whose
// verifier does not match.
type AuthServer struct {
	*httptest.Server
	ClientID, ClientSecret string

	mu          sync.Mutex
	n           int
	codes       map[string]grant
	live        map[string]bool
	revocations []Revocation
}

type grant struct {
	challenge, redirect string
}

// NewAuthServer starts a TLS server, stopped when t ends. Every httptest TLS
// server shares one certificate, so any of their Client()s trusts all.
func NewAuthServer(t *testing.T, clientID, clientSecret string) *AuthServer {
	t.Helper()
	s := &AuthServer{ClientID: clientID, ClientSecret: clientSecret, codes: map[string]grant{}, live: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login/oauth/authorize", s.authorize)
	mux.HandleFunc("GET /oauth/authorize", s.authorize)
	mux.HandleFunc("POST /login/oauth/access_token", s.token)
	mux.HandleFunc("POST /oauth/token", s.token)
	mux.HandleFunc("DELETE /api/v3/applications/{client}/grant", s.revokeGrant)
	mux.HandleFunc("POST /oauth/revoke", s.revokeRFC7009)
	s.Server = httptest.NewTLSServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Live reports whether the server still accepts token.
func (s *AuthServer) Live(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live[token]
}

// Revocations are the revocations accepted so far.
func (s *AuthServer) Revocations() []Revocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Revocation(nil), s.revocations...)
}

func (s *AuthServer) next(prefix string) string {
	s.n++
	return fmt.Sprintf("%s-%s-%d", prefix, s.Listener.Addr().String(), s.n)
}

func (s *AuthServer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != s.ClientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("redirect_uri") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	code := s.next("code")
	s.codes[code] = grant{challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
	s.mu.Unlock()
	to, _ := url.Parse(q.Get("redirect_uri"))
	v := to.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	to.RawQuery = v.Encode()
	http.Redirect(w, r, to.String(), http.StatusFound) //nolint:gosec // G710: a fake authorization server redirects where it is asked, like the real one after checking the client.
}

func (s *AuthServer) client(r *http.Request) bool {
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	return id == s.ClientID && secret == s.ClientSecret
}

func (s *AuthServer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !s.client(r) {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code := r.PostForm.Get("code")
		g, ok := s.codes[code]
		delete(s.codes, code)
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || g.redirect != r.PostForm.Get("redirect_uri") || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
	case "refresh_token":
		rt := r.PostForm.Get("refresh_token")
		if !s.live[rt] {
			oauthError(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		delete(s.live, rt)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	at, rt := s.next("access"), s.next("refresh")
	s.live[at], s.live[rt] = true, true
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": at, "refresh_token": rt, "token_type": "bearer", "expires_in": 28800})
}

func (s *AuthServer) revokeGrant(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if r.PathValue("client") != s.ClientID || !s.client(r) {
		http.Error(w, "bad client", http.StatusUnauthorized)
		return
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.live[body.AccessToken] {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// The whole grant goes: every token this server issued.
	s.live = map[string]bool{}
	s.revocations = append(s.revocations, Revocation{Method: "grant", Token: body.AccessToken})
	w.WriteHeader(http.StatusNoContent)
}

func (s *AuthServer) revokeRFC7009(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !s.client(r) {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := r.PostForm.Get("token")
	delete(s.live, tok)
	s.revocations = append(s.revocations, Revocation{Method: "rfc7009", Token: tok})
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// PersonHeader carries the test browser's person to the fake Dex, which
// signs in whoever it names.
const PersonHeader = "X-Connecttest-Person"

// Dex is a fake identity provider for the manager's pages
// (connect.IdentityProvider), serving its authorization endpoint over TLS.
type Dex struct {
	*httptest.Server
	// RedirectURL is the manager's sign-in route; set once the manager runs.
	RedirectURL string

	mu     sync.Mutex
	n      int
	codes  map[string]dexCode
	tokens map[string]string
}

type dexCode struct{ person, challenge string }

// NewDex starts the fake, stopped when t ends.
func NewDex(t *testing.T) *Dex {
	t.Helper()
	d := &Dex{codes: map[string]dexCode{}, tokens: map[string]string{}}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(d.authorize))
	t.Cleanup(d.Close)
	return d
}

func (d *Dex) authorize(w http.ResponseWriter, r *http.Request) {
	person := r.Header.Get(PersonHeader)
	if person == "" {
		http.Error(w, "nobody signed in", http.StatusUnauthorized)
		return
	}
	d.mu.Lock()
	d.n++
	code := fmt.Sprintf("dex-code-%d", d.n)
	d.codes[code] = dexCode{person: person, challenge: r.URL.Query().Get("code_challenge")}
	d.mu.Unlock()
	http.Redirect(w, r, d.RedirectURL+"?"+url.Values{"code": {code}, "state": {r.URL.Query().Get("state")}}.Encode(), http.StatusFound)
}

// AuthorizationURL implements connect.IdentityProvider.
func (d *Dex) AuthorizationURL(state, codeChallenge, codeChallengeMethod string, _ []string, _ *providers.AuthorizationURLOptions) string {
	return d.URL + "/auth?" + url.Values{"state": {state}, "code_challenge": {codeChallenge}, "code_challenge_method": {codeChallengeMethod}}.Encode()
}

// ExchangeCode implements connect.IdentityProvider.
func (d *Dex) ExchangeCode(_ context.Context, code, verifier string) (*oauth2.Token, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.codes[code]
	delete(d.codes, code)
	if !ok || oauth2.S256ChallengeFromVerifier(verifier) != c.challenge {
		return nil, errors.New("invalid_grant")
	}
	d.n++
	at := fmt.Sprintf("dex-access-%d", d.n)
	d.tokens[at] = c.person
	return &oauth2.Token{AccessToken: at}, nil
}

// ValidateToken implements connect.IdentityProvider.
func (d *Dex) ValidateToken(_ context.Context, accessToken string) (*providers.UserInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	person, ok := d.tokens[accessToken]
	if !ok {
		return nil, errors.New("invalid token")
	}
	return &providers.UserInfo{ID: person}, nil
}

// NewBrowser is a person's browser: it keeps cookies, follows redirects,
// trusts the httptest certificate and is signed in at dex as person.
func NewBrowser(t *testing.T, dex *Dex, person string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	dexHost := dex.Listener.Addr().String()
	base := dex.Client().Transport
	return &http.Client{Jar: jar, Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == dexHost {
			r = r.Clone(r.Context())
			r.Header.Set(PersonHeader, person)
		}
		return base.RoundTrip(r)
	})}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
