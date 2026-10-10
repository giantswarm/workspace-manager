package connect

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"time"

	"github.com/giantswarm/mcp-oauth/providers"
	"golang.org/x/oauth2"
)

// DefaultSessionTTL is how long a browser stays signed in to the manager's
// pages.
const DefaultSessionTTL = time.Hour

// The cookies are Secure and host-only (`__Host-`): no sibling subdomain can
// set one in their place. The manager's base URL is https, or loopback http,
// which browsers treat as secure.
const (
	sessionCookie = "__Host-workspace-manager-session"
	signInCookie  = "__Host-workspace-manager-signin"
	// SignInPath is the manager's redirect URI at the identity provider.
	SignInPath = "/signin"
)

// IdentityProvider signs a browser in to the manager's pages: Dex, the same
// authority the MCP endpoint trusts, so a page's person is the subject the
// tools see. mcp-oauth's dex.Provider is one.
type IdentityProvider interface {
	AuthorizationURL(state, codeChallenge, codeChallengeMethod string, scopes []string, opts *providers.AuthorizationURLOptions) string
	ExchangeCode(ctx context.Context, code, verifier string) (*oauth2.Token, error)
	ValidateToken(ctx context.Context, accessToken string) (*providers.UserInfo, error)
}

// Pages are the person's browser pages: `/connect/<instance>` starts a
// sign-in, `/callback/<instance>` completes it, and `/signin` is where the
// identity provider returns. Both provider pages need a browser signed in to
// the manager, so a callback is completed only by the person its state was
// made for: a link one person hands another connects nobody.
type Pages struct {
	c   *Connector
	idp IdentityProvider
	ttl time.Duration
}

// NewPages builds the pages.
func NewPages(c *Connector, idp IdentityProvider) (*Pages, error) {
	if c == nil || idp == nil {
		return nil, errors.New("connect pages: a connector and an identity provider are required")
	}
	return &Pages{c: c, idp: idp, ttl: DefaultSessionTTL}, nil
}

// session is a browser signed in to the pages.
type session struct {
	Person string `json:"p"`
	Expiry int64  `json:"e"`
}

// signInState is a browser's sign-in at the identity provider, bound to the
// browser by the nonce in its sign-in cookie.
type signInState struct {
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
	Expiry   int64  `json:"e"`
}

// Connect serves `GET /connect/{instance}`: it sends a signed-in browser to
// the provider's authorization URL, and signs any other in first.
func (p *Pages) Connect(w http.ResponseWriter, r *http.Request) {
	instance := r.PathValue("instance")
	if _, err := p.c.instance(instance); err != nil {
		page(w, http.StatusNotFound, "Unknown provider", "This installation has no provider named "+instance+".")
		return
	}
	person, ok := p.person(r)
	if !ok {
		p.signIn(w, r)
		return
	}
	u, _, err := p.c.AuthorizationURL(r.Context(), person, instance)
	if err != nil {
		p.c.log.ErrorContext(r.Context(), "connect page", "instance", instance, "error", err)
		page(w, http.StatusInternalServerError, "Cannot connect "+instance, "The provider's sign-in cannot start right now.")
		return
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

// Callback serves `GET /callback/{instance}`, the provider's redirect.
func (p *Pages) Callback(w http.ResponseWriter, r *http.Request) {
	instance := r.PathValue("instance")
	q := r.URL.Query()
	if _, err := p.c.instance(instance); err != nil {
		page(w, http.StatusNotFound, "Unknown provider", "This installation has no provider named "+instance+".")
		return
	}
	if e := q.Get("error"); e != "" {
		page(w, http.StatusBadRequest, instance+" is not connected", "The provider answered: "+e+".")
		return
	}
	// The state is checked before the browser is sent to sign in, so a
	// forged or stale link goes nowhere.
	if _, err := p.c.CheckState(instance, q.Get("state")); err != nil {
		page(w, http.StatusBadRequest, instance+" is not connected", "This sign-in link is invalid or has expired. Start again from the connect link.")
		return
	}
	person, ok := p.person(r)
	if !ok {
		p.signIn(w, r)
		return
	}
	err := p.c.Complete(r.Context(), person, instance, q.Get("state"), q.Get("code"))
	switch {
	case err == nil:
		page(w, http.StatusOK, instance+" connected", "Your agents can now use "+instance+" as you. You can close this page.")
	case errors.Is(err, ErrInvalidState):
		page(w, http.StatusBadRequest, instance+" is not connected", "This sign-in link is invalid or has expired. Start again from the connect link.")
	case errors.Is(err, ErrOtherPerson):
		page(w, http.StatusForbidden, instance+" is not connected", "This sign-in link was made for another person. Start from your own connect link.")
	default:
		p.c.log.WarnContext(r.Context(), "sign-in not completed", "instance", instance, "error", err)
		page(w, http.StatusBadGateway, instance+" is not connected", "The provider did not complete the sign-in. Start again from the connect link.")
	}
}

// SignIn serves `GET /signin`, where the identity provider returns: it
// checks the state against the browser's sign-in cookie, redeems the code,
// sets the session and returns to the page that asked.
func (p *Pages) SignIn(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var st signInState
	cookie, err := r.Cookie(signInCookie)
	if err != nil || open(p.c.keyring, signInContext, q.Get("state"), &st) != nil ||
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(st.Nonce)) != 1 ||
		!p.c.now().Before(time.Unix(st.Expiry, 0)) {
		page(w, http.StatusBadRequest, "Sign-in failed", "This sign-in is invalid or has expired. Open the connect link again.")
		return
	}
	p.setCookie(w, signInCookie, "", -1)
	if e := q.Get("error"); e != "" {
		page(w, http.StatusUnauthorized, "Sign-in failed", "The identity provider answered: "+e+".")
		return
	}
	tok, err := p.idp.ExchangeCode(r.Context(), q.Get("code"), st.Verifier)
	if err != nil {
		p.c.log.WarnContext(r.Context(), "page sign-in: code not redeemed", "error", providerError(err))
		page(w, http.StatusUnauthorized, "Sign-in failed", "The identity provider did not complete the sign-in. Open the connect link again.")
		return
	}
	info, err := p.idp.ValidateToken(r.Context(), tok.AccessToken)
	if err != nil || info == nil || info.ID == "" {
		page(w, http.StatusUnauthorized, "Sign-in failed", "The identity provider did not name you. Open the connect link again.")
		return
	}
	value, err := seal(p.c.keyring, sessionContext, session{Person: info.ID, Expiry: p.c.now().Add(p.ttl).Unix()})
	if err != nil {
		page(w, http.StatusInternalServerError, "Sign-in failed", "The session cannot be kept right now.")
		return
	}
	p.setCookie(w, sessionCookie, value, int(p.ttl.Seconds()))
	http.Redirect(w, r, st.Return, http.StatusSeeOther)
}

// person is the browser's signed-in person, if its session is valid.
func (p *Pages) person(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	var s session
	if open(p.c.keyring, sessionContext, cookie.Value, &s) != nil || s.Person == "" || !p.c.now().Before(time.Unix(s.Expiry, 0)) {
		return "", false
	}
	return s.Person, true
}

// signIn sends the browser to the identity provider, to return to the
// request's own URL.
func (p *Pages) signIn(w http.ResponseWriter, r *http.Request) {
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		page(w, http.StatusInternalServerError, "Sign-in failed", "The sign-in cannot start right now.")
		return
	}
	st := signInState{
		Nonce:    base64.RawURLEncoding.EncodeToString(nonce),
		Verifier: oauth2.GenerateVerifier(),
		Return:   r.URL.RequestURI(),
		Expiry:   p.c.now().Add(p.c.ttl).Unix(),
	}
	state, err := seal(p.c.keyring, signInContext, st)
	if err != nil {
		page(w, http.StatusInternalServerError, "Sign-in failed", "The sign-in cannot start right now.")
		return
	}
	p.setCookie(w, signInCookie, st.Nonce, int(p.c.ttl.Seconds()))
	http.Redirect(w, r, p.idp.AuthorizationURL(state, oauth2.S256ChallengeFromVerifier(st.Verifier), "S256", nil, nil), http.StatusSeeOther)
}

// setCookie sets an HttpOnly cookie for the whole host. SameSite Lax,
// because the identity provider's and the provider's redirects are
// cross-site top-level navigations that must carry it.
func (*Pages) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>body{font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;line-height:1.5}</style>
</head><body><h1>{{.Title}}</h1><p>{{.Message}}</p></body></html>
`))

func page(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// A callback's URL carries the code: no page passes it on.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, struct{ Title, Message string }{title, message})
}

// NotConfigured serves the pages' routes when the manager has no provider
// sign-in: no provider instance, or no --enable-oauth to name the person. A
// person following a connect link learns why instead of reading a 404.
func NotConfigured(w http.ResponseWriter, _ *http.Request) {
	page(w, http.StatusServiceUnavailable, "No provider configured",
		"This workspace-manager has no provider sign-in: it needs at least one provider instance (--providers-config) and --enable-oauth.")
}
