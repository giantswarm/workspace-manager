// Package github is the GitHub provider kind: github.com or a GitHub
// Enterprise Server. A GitHub App is the provider on an installation: its
// installation token on an owner is the sync credential, the installation's
// repositories are the listing, and its user-to-server token, muster's grant
// of the person, is the person's token. Only internal/provider/kinds imports
// this package.
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
	"golang.org/x/sync/errgroup"

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
}

// AppValues identify the GitHub App.
type AppValues struct {
	// ID is the App's ID or client ID, the issuer of its JWTs.
	ID string `json:"id"`
	// PrivateKey is the App's PEM private key.
	PrivateKey provider.SecretRef `json:"privateKey"`
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
		"app.id":              v.App.ID == "",
		"app.privateKey.name": v.App.PrivateKey.Name == "",
		"app.privateKey.key":  v.App.PrivateKey.Key == "",
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
	return []provider.SecretRef{k.v.App.PrivateKey}
}

// Person implements provider.Kind: the login GET /user answers for the
// person's user-to-server token.
func (k *Kind) Person(ctx context.Context, cred oauth2.TokenSource) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if err := k.get(ctx, cred, "/user", &user); err != nil {
		return "", err
	}
	if user.Login == "" {
		return "", errors.New("GET /user: no login in the answer")
	}
	return user.Login, nil
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
// sync can fetch; from the listing cache within provider.ListingFreshness, unless
// ctx asks for a provider.FreshListing.
func (k *Kind) List(ctx context.Context, cred oauth2.TokenSource, owner string) ([]provider.Item, error) {
	return k.cache.Get(ctx, owner, func() ([]provider.Item, error) { return k.list(ctx, cred, owner) })
}

// list pages through the installation's repositories, perPage a call.
func (k *Kind) list(ctx context.Context, cred oauth2.TokenSource, owner string) ([]provider.Item, error) {
	type page struct {
		Repositories []repository `json:"repositories"`
	}
	pages, err := getPages[page](ctx, k, cred, k.api.String()+"/installation/repositories?per_page="+strconv.Itoa(perPage))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", owner, err)
	}
	var items []provider.Item
	for _, p := range pages {
		for _, r := range p.Repositories {
			if !strings.EqualFold(r.Owner.Login, owner) {
				return nil, fmt.Errorf("listing %s: the credential is the App's installation on %s", owner, r.Owner.Login)
			}
			items = append(items, r.item(owner))
		}
	}
	return items, nil
}

// perPage is a listing page's size, GitHub's maximum. Up to as many named
// repositories cost fewer requests asked for one by one than one listing of
// an owner with more repositories than a page holds.
const perPage = 100

// concurrency bounds the requests one call has in flight: the pages of a
// listing, the repositories asked for one by one.
const concurrency = 8

// Readable implements provider.Kind with the person's user-to-server token,
// which reads what both the person and the App's installation can. Up to
// perPage names are each asked for on their own, concurrently, so the check
// costs what the workspace selects, not what the owner holds. More names
// take one listing of the owner's repositories as an organization, its pages
// fetched concurrently; on a user account, whose listing shows only public
// repositories, each named repository the listing left out is asked for on
// its own. A repository GitHub answers 404 or 403 for is one the person
// cannot read.
func (k *Kind) Readable(ctx context.Context, cred oauth2.TokenSource, owner string, names []string) ([]provider.Item, error) {
	if len(names) <= perPage {
		return k.readEach(ctx, cred, owner, names)
	}
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
	var rest []string
	for _, n := range names {
		if wanted[strings.ToLower(n)] {
			rest = append(rest, n)
		}
	}
	more, err := k.readEach(ctx, cred, owner, rest)
	return append(items, more...), err
}

// readEach asks for each of owner's repositories named in names on its own,
// concurrently: those the person reads, in the order of names.
func (k *Kind) readEach(ctx context.Context, cred oauth2.TokenSource, owner string, names []string) ([]provider.Item, error) {
	found := make([]*provider.Item, len(names))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i, n := range names {
		g.Go(func() error {
			var r repository
			err := k.get(gctx, cred, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(n), &r)
			var se *statusError
			switch {
			case errors.As(err, &se) && !se.limited && (se.status == http.StatusNotFound || se.status == http.StatusForbidden):
				return nil
			case err != nil:
				return fmt.Errorf("reading %s/%s: %w", owner, n, err)
			}
			it := r.item(owner)
			found[i] = &it
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	var items []provider.Item
	for _, it := range found {
		if it != nil {
			items = append(items, *it)
		}
	}
	return items, nil
}

// listAs pages through owner's repositories as cred sees them: an
// organization's every type, else, on a 404, a user account's.
func (k *Kind) listAs(ctx context.Context, cred oauth2.TokenSource, owner string) ([]provider.Item, bool, error) {
	query := "/repos?type=all&per_page=" + strconv.Itoa(perPage)
	items, err := k.listPages(ctx, cred, owner, "/orgs/"+url.PathEscape(owner)+query)
	var se *statusError
	if !errors.As(err, &se) || se.status != http.StatusNotFound {
		return items, false, err
	}
	items, err = k.listPages(ctx, cred, owner, "/users/"+url.PathEscape(owner)+query)
	return items, true, err
}

func (k *Kind) listPages(ctx context.Context, cred oauth2.TokenSource, owner, path string) ([]provider.Item, error) {
	pages, err := getPages[[]repository](ctx, k, cred, k.api.String()+path)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", owner, err)
	}
	var items []provider.Item
	for _, page := range pages {
		for _, r := range page {
			items = append(items, r.item(owner))
		}
	}
	return items, nil
}

// getPages GETs every page of the listing at first, in order. The first
// page's `Link` header names the last one: the pages between are fetched
// concurrently, by their number. Without a last link the pages are followed
// one after the other.
func getPages[P any](ctx context.Context, k *Kind, cred oauth2.TokenSource, first string) ([]P, error) {
	pages := make([]P, 1)
	header, err := k.getPage(ctx, cred, first, &pages[0])
	if err != nil {
		return nil, err
	}
	last, n := lastLink(header.Get("Link"))
	if n == 0 {
		for next := nextLink(header.Get("Link")); next != ""; next = nextLink(header.Get("Link")) {
			pages = append(pages, *new(P))
			if header, err = k.getPage(ctx, cred, next, &pages[len(pages)-1]); err != nil {
				return nil, err
			}
		}
		return pages, nil
	}
	pages = slices.Grow(pages, n-1)[:n]
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for i := 2; i <= n; i++ {
		u := *last
		q := u.Query()
		q.Set("page", strconv.Itoa(i))
		u.RawQuery = q.Encode()
		g.Go(func() error {
			_, err := k.getPage(gctx, cred, u.String(), &pages[i-1])
			return err
		})
	}
	return pages, g.Wait()
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

// getPage GETs one page and returns the answer's header, which links the
// listing's other pages.
func (k *Kind) getPage(ctx context.Context, cred oauth2.TokenSource, u string, out any) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if cred != nil {
		tok, err := cred.Token()
		if err != nil {
			return nil, fmt.Errorf("credential: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	return k.do(req, http.StatusOK, out)
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
	return link(header, "next")
}

// lastLink is rel="last" of a Link header and the page number it carries; 0
// without one.
func lastLink(header string) (*url.URL, int) {
	u, err := url.Parse(link(header, "last"))
	if err != nil || u.Host == "" {
		return nil, 0
	}
	n, err := strconv.Atoi(u.Query().Get("page"))
	if err != nil || n < 2 {
		return nil, 0
	}
	return u, n
}

// link extracts the URL of relation rel from a Link header, empty without
// one.
func link(header, rel string) string {
	for _, part := range strings.Split(header, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		for _, s := range segs[1:] {
			if strings.TrimSpace(s) == `rel="`+rel+`"` {
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
