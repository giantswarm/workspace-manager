// Package github is the GitHub provider kind: github.com or a GitHub
// Enterprise Server. A GitHub App is the provider on an installation: its
// installation token on an owner is the sync credential, the installation's
// repositories are the listing, and its user-to-server sign-in is the person's
// sign-in. Only internal/provider/kinds imports this package.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/provider"
)

// KindName is the kind's name in the provider configuration.
const KindName = "github"

// Values are an instance's settings.
type Values struct {
	// URL is the web URL: https://github.com (the default) or a GitHub
	// Enterprise Server's. It is the git host.
	URL string `json:"url,omitempty"`
	// APIURL is the REST API's base URL. Empty derives it from URL:
	// https://api.github.com for github.com, `<url>/api/v3` otherwise.
	APIURL string `json:"apiURL,omitempty"`
	// App is the GitHub App the sync authenticates as.
	App AppValues `json:"app"`
	// OAuth is the App's OAuth client, for the person's sign-in.
	OAuth OAuthValues `json:"oauth"`
}

// AppValues identify the GitHub App.
type AppValues struct {
	// ID is the App's ID or client ID, the issuer of its JWTs.
	ID string `json:"id"`
	// PrivateKey is the App's PEM private key.
	PrivateKey provider.SecretRef `json:"privateKey"`
}

// OAuthValues are the App's OAuth client.
type OAuthValues struct {
	ClientID     string             `json:"clientID"`
	ClientSecret provider.SecretRef `json:"clientSecret"`
}

// Options are an instance's dependencies beyond its Values.
type Options struct {
	// HTTP is the client the API calls use; nil for http.DefaultClient.
	HTTP *http.Client
	// Clock is what the instance tells time and waits by; nil for the wall
	// clock.
	Clock provider.Clock
}

// Factory builds GitHub instances with o.
func Factory(o Options) provider.Factory {
	return func(raw json.RawMessage) (provider.Kind, error) {
		var v Values
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("values: %w", err)
		}
		return New(v, o)
	}
}

// Kind is a configured GitHub instance.
type Kind struct {
	v     Values
	web   *url.URL
	api   *url.URL
	http  *http.Client
	clock provider.Clock
	cache *provider.ListingCache
}

// New validates v and builds the instance.
func New(v Values, o Options) (*Kind, error) {
	if v.URL == "" {
		v.URL = "https://github.com"
	}
	web, err := parseBase("url", v.URL)
	if err != nil {
		return nil, err
	}
	if v.APIURL == "" {
		if web.Host == "github.com" {
			v.APIURL = "https://api.github.com"
		} else {
			v.APIURL = strings.TrimSuffix(web.String(), "/") + "/api/v3"
		}
	}
	api, err := parseBase("apiURL", v.APIURL)
	if err != nil {
		return nil, err
	}
	var missing []string
	for field, empty := range map[string]bool{
		"app.id":                  v.App.ID == "",
		"app.privateKey.name":     v.App.PrivateKey.Name == "",
		"app.privateKey.key":      v.App.PrivateKey.Key == "",
		"oauth.clientID":          v.OAuth.ClientID == "",
		"oauth.clientSecret.name": v.OAuth.ClientSecret.Name == "",
		"oauth.clientSecret.key":  v.OAuth.ClientSecret.Key == "",
	} {
		if empty {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, fmt.Errorf("values: %s required", strings.Join(missing, ", "))
	}
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
	if o.Clock == nil {
		o.Clock = provider.RealClock{}
	}
	return &Kind{v: v, web: web, api: api, http: o.HTTP, clock: o.Clock, cache: provider.NewListingCache(o.Clock)}, nil
}

func parseBase(field, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(raw, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("values: %s must be an absolute http(s) URL, got %q", field, raw)
	}
	return u, nil
}

// SecretRefs implements provider.Kind.
func (k *Kind) SecretRefs() []provider.SecretRef {
	return []provider.SecretRef{k.v.App.PrivateKey, k.v.OAuth.ClientSecret}
}

// SignIn implements provider.Kind: a GitHub App's user-to-server sign-in,
// which takes the App's permissions rather than scopes.
func (k *Kind) SignIn() provider.SignIn {
	web := k.web.String()
	return provider.SignIn{
		AuthURL:  web + "/login/oauth/authorize",
		TokenURL: web + "/login/oauth/access_token",
		// DELETE with the client's Basic credentials and the token in the
		// body revokes the person's grant.
		RevocationURL: k.api.String() + "/applications/" + url.PathEscape(k.v.OAuth.ClientID) + "/grant",
		Revocation:    provider.RevokeGrant,
		ClientID:      k.v.OAuth.ClientID,
		ClientSecret:  k.v.OAuth.ClientSecret,
	}
}

// Hosts implements provider.Kind. git takes a token as the password of any
// user; `x-access-token` is the name GitHub documents for App tokens.
func (k *Kind) Hosts() provider.Hosts {
	h := provider.Hosts{
		Git: []provider.Host{{Name: k.web.Host, Scheme: provider.SchemeBasic, User: "x-access-token"}},
		CLI: "gh",
	}
	// A GitHub Enterprise Server serves its API under the git host
	// (`/api/v3`): one host takes one credential, and GitHub's API takes the
	// git host's Basic token too.
	if k.api.Host != k.web.Host {
		h.API = []provider.Host{{Name: k.api.Host, Scheme: provider.SchemeBearer}}
	}
	return h
}

// List implements provider.Kind: the repositories of the App's installation
// on owner, which is what cred, the owner's SyncCredential, can see and the
// sync can fetch; from the listing cache within provider.ListingFreshness.
func (k *Kind) List(ctx context.Context, cred oauth2.TokenSource, owner string) ([]provider.Item, error) {
	return k.cache.Get(owner, func() ([]provider.Item, error) { return k.list(ctx, cred, owner) })
}

// list pages through the installation's repositories, 100 a call.
func (k *Kind) list(ctx context.Context, cred oauth2.TokenSource, owner string) ([]provider.Item, error) {
	var items []provider.Item
	for next := k.api.String() + "/installation/repositories?per_page=100"; next != ""; {
		var page struct {
			Repositories []repository `json:"repositories"`
		}
		var err error
		if next, err = k.getPage(ctx, cred, next, &page); err != nil {
			return nil, fmt.Errorf("listing %s: %w", owner, err)
		}
		for _, r := range page.Repositories {
			if !strings.EqualFold(r.Owner.Login, owner) {
				return nil, fmt.Errorf("listing %s: the credential is the App's installation on %s", owner, r.Owner.Login)
			}
			items = append(items, r.item(owner))
		}
	}
	return items, nil
}

// Readable implements provider.Kind with the person's user-to-server token,
// which reads what both the person and the App's installation can: one
// listing of the owner's repositories as an organization; on a user account,
// whose listing shows only public repositories, each named repository the
// listing left out is asked for on its own. A repository GitHub answers 404
// or 403 for is one the person cannot read.
func (k *Kind) Readable(ctx context.Context, cred oauth2.TokenSource, owner string, names []string) ([]provider.Item, error) {
	listed, isUser, err := k.listAs(ctx, cred, owner)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[strings.ToLower(n)] = true
	}
	var items []provider.Item
	for _, it := range listed {
		if wanted[strings.ToLower(it.Name)] {
			items = append(items, it)
			delete(wanted, strings.ToLower(it.Name))
		}
	}
	if !isUser {
		return items, nil
	}
	for _, n := range names {
		if !wanted[strings.ToLower(n)] {
			continue
		}
		var r repository
		err := k.get(ctx, cred, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(n), &r)
		var se *statusError
		switch {
		case errors.As(err, &se) && !se.limited && (se.status == http.StatusNotFound || se.status == http.StatusForbidden):
			continue
		case err != nil:
			return nil, fmt.Errorf("reading %s/%s: %w", owner, n, err)
		}
		items = append(items, r.item(owner))
	}
	return items, nil
}

// listAs pages through owner's repositories as cred sees them: an
// organization's every type, else, on a 404, a user account's.
func (k *Kind) listAs(ctx context.Context, cred oauth2.TokenSource, owner string) ([]provider.Item, bool, error) {
	items, err := k.listPages(ctx, cred, owner, "/orgs/"+url.PathEscape(owner)+"/repos?type=all&per_page=100")
	var se *statusError
	if !errors.As(err, &se) || se.status != http.StatusNotFound {
		return items, false, err
	}
	items, err = k.listPages(ctx, cred, owner, "/users/"+url.PathEscape(owner)+"/repos?type=all&per_page=100")
	return items, true, err
}

func (k *Kind) listPages(ctx context.Context, cred oauth2.TokenSource, owner, path string) ([]provider.Item, error) {
	var items []provider.Item
	for next := k.api.String() + path; next != ""; {
		var page []repository
		var err error
		if next, err = k.getPage(ctx, cred, next, &page); err != nil {
			return nil, fmt.Errorf("listing %s: %w", owner, err)
		}
		for _, r := range page {
			items = append(items, r.item(owner))
		}
	}
	return items, nil
}

// repository is what the listing reads of a repository; `size` is in KiB.
type repository struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	Language      string    `json:"language"`
	Topics        []string  `json:"topics"`
	Archived      bool      `json:"archived"`
	Fork          bool      `json:"fork"`
	PushedAt      time.Time `json:"pushed_at"`
	Size          int64     `json:"size"`
	CloneURL      string    `json:"clone_url"`
	DefaultBranch string    `json:"default_branch"`
}

func (r repository) item(owner string) provider.Item {
	return provider.Item{
		Owner:         owner,
		Name:          r.Name,
		Language:      r.Language,
		Topics:        r.Topics,
		Archived:      r.Archived,
		Fork:          r.Fork,
		LastChange:    r.PushedAt,
		SizeKiB:       r.Size,
		CloneURL:      r.CloneURL,
		DefaultBranch: r.DefaultBranch,
	}
}

func (k *Kind) get(ctx context.Context, cred oauth2.TokenSource, path string, out any) error {
	_, err := k.getPage(ctx, cred, k.api.String()+path, out)
	return err
}

// getPage GETs one page and returns the URL of the next one, empty on the
// last.
func (k *Kind) getPage(ctx context.Context, cred oauth2.TokenSource, u string, out any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	if cred != nil {
		tok, err := cred.Token()
		if err != nil {
			return "", fmt.Errorf("credential: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	header, err := k.do(req, http.StatusOK, out)
	if err != nil {
		return "", err
	}
	return nextLink(header.Get("Link")), nil
}

// A rate limit is waited out as GitHub says: `Retry-After` on a secondary
// limit, the primary limit's reset time once `x-ratelimit-remaining` is 0,
// else a minute. A wait beyond maxRateLimitWait, or more than rateLimitRetries
// of them, fails the call with the limit instead, for the caller's next cycle.
const (
	maxRateLimitWait = 5 * time.Minute
	rateLimitRetries = 3
)

// do sends req (which carries no body, so it is sent again after a rate
// limit's wait) and decodes an answer of status want into out.
func (k *Kind) do(req *http.Request, want int, out any) (http.Header, error) {
	for attempt := 0; ; attempt++ {
		header, err := k.once(req, want, out)
		var se *statusError
		if !errors.As(err, &se) || !se.limited || attempt == rateLimitRetries || se.resetIn > maxRateLimitWait {
			return header, err
		}
		if err := k.clock.Sleep(req.Context(), se.resetIn); err != nil {
			return nil, fmt.Errorf("%w (while waiting out a rate limit: %w)", err, se)
		}
	}
}

func (k *Kind) once(req *http.Request, want int, out any) (http.Header, error) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		se := &statusError{method: req.Method, path: req.URL.Path, status: resp.StatusCode, message: e.Message}
		se.resetIn, se.limited = rateLimitWait(resp, e.Message, k.clock.Now())
		return nil, se
	}
	return resp.Header, json.NewDecoder(resp.Body).Decode(out)
}

// rateLimitWait reads a rate-limited answer's wait: false for any other
// answer.
func rateLimitWait(resp *http.Response, message string, now time.Time) (time.Duration, bool) {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return time.Duration(s) * time.Second, true
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			// A second past the reset, so the retry lands after it.
			return max(time.Unix(reset, 0).Sub(now), 0) + time.Second, true
		}
	}
	if strings.Contains(strings.ToLower(message), "rate limit") {
		return time.Minute, true
	}
	return 0, false
}

// nextLink extracts rel="next" from a Link header.
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(segs[0]), "<>")
			}
		}
	}
	return ""
}

// statusError is an API answer other than the one expected; a rate limit
// says when it resets.
type statusError struct {
	method, path string
	status       int
	message      string
	limited      bool
	resetIn      time.Duration
}

// Unwrap makes a 401 an ErrCredentialRefused.
func (e *statusError) Unwrap() error {
	if e.status == http.StatusUnauthorized {
		return provider.ErrCredentialRefused
	}
	return nil
}

func (e *statusError) Error() string {
	s := fmt.Sprintf("%s %s: %d %s %s", e.method, e.path, e.status, http.StatusText(e.status), e.message)
	if e.limited {
		s += fmt.Sprintf(" (rate limit resets in %s)", e.resetIn)
	}
	return s
}
