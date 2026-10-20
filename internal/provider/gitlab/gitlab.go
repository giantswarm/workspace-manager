// Package gitlab is the GitLab provider kind: gitlab.com or a self-managed
// GitLab. A group access token is the sync credential, a group's projects
// (its sub-groups' included) or a user namespace's are the listing, and the
// person's token is an OAuth token of the person. Only internal/provider/kinds
// imports this package.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"

	"github.com/giantswarm/workspace-manager/internal/provider"
)

// KindName is the kind's name in the provider configuration.
const KindName = "gitlab"

// SubgroupSeparator joins a project's sub-group path to its name: a
// project's name within its owner is one path segment (it is a directory of
// the volume), and GitLab allows the character in no group or project path,
// so `<owner>/team/api` is the name `team~api` and no two projects share one.
const SubgroupSeparator = "~"

// Values are an instance's settings.
type Values struct {
	// URL is the web URL: https://gitlab.com (the default) or a self-managed
	// GitLab's. It is the git host.
	URL string `json:"url,omitempty"`
	// APIURL is the REST API's base URL. Empty derives it from URL:
	// `<url>/api/v4`.
	APIURL string `json:"apiURL,omitempty"`
	// Token is the sync credential: a group access token with `read_api` and
	// `read_repository`.
	Token provider.SecretRef `json:"token"`
	// OAuth is the retired OAuth application of the person's sign-in,
	// ignored: the person's token is muster's grant. Accepted so a deployment
	// that still configures it starts.
	OAuth json.RawMessage `json:"oauth,omitempty"`
}

// Options are an instance's dependencies beyond its Values.
type Options struct {
	// HTTP is the client the API calls use; nil for http.DefaultClient.
	HTTP *http.Client
	// Clock is what the instance tells time and waits by; nil for the wall
	// clock.
	Clock provider.Clock
	// Logger takes what a listing survives: a head commit it could not read;
	// nil for slog.Default().
	Logger *slog.Logger
}

// Factory builds GitLab instances with o.
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

// Kind is a configured GitLab instance.
type Kind struct {
	v     Values
	web   *url.URL
	api   *url.URL
	http  *http.Client
	clock provider.Clock
	log   *slog.Logger
	cache *provider.ListingCache
	langs *byActivity[string]
	heads *byActivity[time.Time]
}

// New validates v and builds the instance.
func New(v Values, o Options) (*Kind, error) {
	if v.URL == "" {
		v.URL = "https://gitlab.com"
	}
	web, err := parseBase("url", v.URL)
	if err != nil {
		return nil, err
	}
	if v.APIURL == "" {
		v.APIURL = web.String() + "/api/v4"
	}
	api, err := parseBase("apiURL", v.APIURL)
	if err != nil {
		return nil, err
	}
	if api.Host == web.Host && apiPrefix(api) == "/" {
		return nil, fmt.Errorf("values: apiURL on the git host must lie below a directory of its own, as `<url>/api/v4`, got %q", v.APIURL)
	}
	var missing []string
	for field, empty := range map[string]bool{
		"token.name": v.Token.Name == "",
		"token.key":  v.Token.Key == "",
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
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Kind{
		v: v, web: web, api: api, http: o.HTTP, clock: o.Clock, log: o.Logger,
		cache: provider.NewListingCache(o.Clock), langs: newByActivity[string](), heads: newByActivity[time.Time](),
	}, nil
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
	return []provider.SecretRef{k.v.Token}
}

// Person implements provider.Kind: the username GET /user answers for the
// person's OAuth token.
func (k *Kind) Person(ctx context.Context, cred oauth2.TokenSource) (string, error) {
	var user struct {
		Username string `json:"username"`
	}
	if err := k.get(ctx, cred, "/user", &user); err != nil {
		return "", err
	}
	if user.Username == "" {
		return "", errors.New("GET /user: no username in the answer")
	}
	return user.Username, nil
}

// Owners implements provider.Kind: the person's user namespace and the
// top-level groups they are a member of, as their OAuth token sees them.
func (k *Kind) Owners(ctx context.Context, cred oauth2.TokenSource) ([]provider.Owner, error) {
	user, err := k.Person(ctx, cred)
	if err != nil {
		return nil, err
	}
	owners := []provider.Owner{{Name: user, Kind: provider.OwnerUser}}
	q := url.Values{"top_level_only": {"true"}, "min_access_level": {"10"}, "per_page": {strconv.Itoa(perPage)}, "order_by": {"path"}}
	for next := k.api.String() + "/groups?" + q.Encode(); next != ""; {
		var groups []struct {
			FullPath string `json:"full_path"`
		}
		header, err := k.getPage(ctx, cred, next, &groups)
		if err != nil {
			return nil, fmt.Errorf("listing the owners: %w", err)
		}
		for _, g := range groups {
			owners = append(owners, provider.Owner{Name: g.FullPath, Kind: provider.OwnerGroup})
		}
		next = link(header.Get("Link"), "next")
	}
	return owners, nil
}

// Hosts implements provider.Kind. GitLab takes an OAuth token over git as
// the password of the user `oauth2`, and on its API as a Bearer token, never
// as Basic. A GitLab serving its API under the git host lists the API's
// directory (`/api/`, which holds the REST `v4` and `graphql`) as a path
// prefix of the git host with Bearer.
func (k *Kind) Hosts() provider.Hosts {
	h := provider.Hosts{
		Git: []provider.Host{{Name: k.web.Host, Scheme: provider.SchemeBasic, User: "oauth2"}},
		API: []provider.Host{{Name: k.api.Host, Scheme: provider.SchemeBearer}},
		CLI: "glab",
	}
	if k.api.Host == k.web.Host {
		h.API[0].PathPrefix = apiPrefix(k.api)
	}
	return h
}

// apiPrefix is the directory of the API's base URL: `/api/` of `/api/v4`.
func apiPrefix(api *url.URL) string {
	return strings.TrimSuffix(path.Dir(path.Clean("/"+api.Path)), "/") + "/"
}

// SyncCredential implements provider.Kind: the group access token from its
// Secret, read here, so a missing Secret fails the first sync of the owner
// with its name.
func (k *Kind) SyncCredential(ctx context.Context, secrets provider.Secrets, _ string) (oauth2.TokenSource, error) {
	tok, err := secrets.Value(ctx, k.v.Token)
	if err != nil {
		return nil, fmt.Errorf("the group access token (Secret %s): %w", k.v.Token, err)
	}
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: strings.TrimSpace(string(tok)), TokenType: "Bearer"}), nil
}

// List implements provider.Kind: owner's projects that cred, the owner's
// SyncCredential, can see, each with its primary language and its head's
// date; from the listing cache within provider.ListingFreshness, unless ctx
// asks for a provider.FreshListing.
func (k *Kind) List(ctx context.Context, cred oauth2.TokenSource, owner string) ([]provider.Item, error) {
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	return k.cache.Get(ctx, owner, func() ([]provider.Item, error) {
		projects, err := k.listAs(ctx, cred, owner, true)
		if err != nil {
			return nil, err
		}
		return k.items(ctx, cred, owner, projects, true)
	})
}

// Readable implements provider.Kind with the person's own token. Up to
// perPage names are each asked for on their own (`/projects/<path>`),
// concurrently, so the check costs what the workspace selects, not what the
// owner holds. More names take one listing of the owner's projects as the
// person sees them, its pages fetched concurrently, which GitLab filters by
// what they can read; an owner the person cannot see at all (GitLab's 404)
// holds nothing they read. A project GitLab answers 404 or 403 for is one the
// person cannot read. Readable reports no language; a named project's head is
// read with the person's token unless known at its last activity.
func (k *Kind) Readable(ctx context.Context, cred oauth2.TokenSource, owner string, names []string) ([]provider.Item, error) {
	if err := checkOwner(owner); err != nil {
		return nil, err
	}
	if len(names) <= perPage {
		named, err := k.readEach(ctx, cred, owner, names)
		if err != nil {
			return nil, err
		}
		return k.items(ctx, cred, owner, named, false)
	}
	projects, err := k.listAs(ctx, cred, owner, false)
	var se *statusError
	switch {
	case errors.As(err, &se) && se.status == http.StatusNotFound:
		return nil, nil
	case err != nil:
		return nil, err
	}
	wanted := make(map[string]bool, len(names))
	for _, n := range names {
		wanted[strings.ToLower(n)] = true
	}
	var named []project
	for _, p := range projects {
		if wanted[strings.ToLower(p.name(owner))] {
			named = append(named, p)
		}
	}
	return k.items(ctx, cred, owner, named, false)
}

// readEach asks for each of owner's projects named in names on its own,
// concurrently: those the person reads, in the order of names. A project
// with nothing to mirror, or found elsewhere than under its name (GitLab
// follows a renamed project's old path), is one the listing would not have
// named either.
func (k *Kind) readEach(ctx context.Context, cred oauth2.TokenSource, owner string, names []string) ([]project, error) {
	found := make([]*project, len(names))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(projectLookups)
	for i, n := range names {
		g.Go(func() error {
			var p project
			full := owner + "/" + strings.ReplaceAll(n, SubgroupSeparator, "/")
			err := k.get(gctx, cred, "/projects/"+url.PathEscape(full), &p)
			var se *statusError
			switch {
			case errors.As(err, &se) && !se.limited && (se.status == http.StatusNotFound || se.status == http.StatusForbidden):
				return nil
			case err != nil:
				return fmt.Errorf("reading %s: %w", full, err)
			}
			if p.EmptyRepo || p.DefaultBranch == "" || !p.under(owner) || !strings.EqualFold(p.name(owner), n) {
				return nil
			}
			found[i] = &p
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	var projects []project
	for _, p := range found {
		if p != nil {
			projects = append(projects, *p)
		}
	}
	return projects, nil
}

// checkOwner refuses a sub-group as an owner: an owner is one segment of the
// volume's layout, and a group's listing takes its sub-groups' projects.
func checkOwner(owner string) error {
	if owner == "" || strings.Contains(owner, "/") {
		return fmt.Errorf("owner %q: a GitLab owner is a top-level group or a user, whose sub-groups' projects it lists", owner)
	}
	return nil
}

// listAs pages through owner's projects as cred sees them, sub-groups
// included and projects shared into the group left out: a group's, else, on
// a 404, a user namespace's. Statistics (the repository size) are asked for
// with the sync credential only, which may read them.
func (k *Kind) listAs(ctx context.Context, cred oauth2.TokenSource, owner string, statistics bool) ([]project, error) {
	q := url.Values{"per_page": {strconv.Itoa(perPage)}, "order_by": {"id"}, "sort": {"asc"}}
	if statistics {
		q.Set("statistics", "true")
	}
	gq := url.Values{"include_subgroups": {"true"}, "with_shared": {"false"}}
	for key, vs := range q {
		gq[key] = vs
	}
	projects, err := k.listPages(ctx, cred, owner, "/groups/"+url.PathEscape(owner)+"/projects?"+gq.Encode())
	var se *statusError
	if !errors.As(err, &se) || se.status != http.StatusNotFound {
		return projects, err
	}
	return k.listPages(ctx, cred, owner, "/users/"+url.PathEscape(owner)+"/projects?"+q.Encode())
}

func (k *Kind) listPages(ctx context.Context, cred oauth2.TokenSource, owner, path string) ([]project, error) {
	pages, err := k.getPages(ctx, cred, k.api.String()+path)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", owner, err)
	}
	var out []project
	for _, page := range pages {
		for _, p := range page {
			// An empty project has nothing to mirror; a project outside the
			// owner's namespace is not the owner's.
			if p.EmptyRepo || p.DefaultBranch == "" || !p.under(owner) {
				continue
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// getPages GETs every page of the listing at first, in order. The first
// page's `Link` header names the last one: the pages between are fetched
// concurrently, by their number. Without a last link (GitLab leaves it out
// past 10,000 projects) the pages are followed one after the other.
func (k *Kind) getPages(ctx context.Context, cred oauth2.TokenSource, first string) ([][]project, error) {
	pages := make([][]project, 1)
	header, err := k.getPage(ctx, cred, first, &pages[0])
	if err != nil {
		return nil, err
	}
	last, n := lastLink(header.Get("Link"))
	if n == 0 {
		for next := link(header.Get("Link"), "next"); next != ""; next = link(header.Get("Link"), "next") {
			pages = append(pages, nil)
			if header, err = k.getPage(ctx, cred, next, &pages[len(pages)-1]); err != nil {
				return nil, err
			}
		}
		return pages, nil
	}
	pages = slices.Grow(pages, n-1)[:n]
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(projectLookups)
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

// project is what the listing reads of a project; the repository size is in
// bytes.
type project struct {
	ID                int64     `json:"id"`
	Path              string    `json:"path"`
	PathWithNamespace string    `json:"path_with_namespace"`
	Topics            []string  `json:"topics"`
	Archived          bool      `json:"archived"`
	ForkedFrom        *struct{} `json:"forked_from_project"`
	LastActivityAt    time.Time `json:"last_activity_at"`
	HTTPURLToRepo     string    `json:"http_url_to_repo"`
	DefaultBranch     string    `json:"default_branch"`
	EmptyRepo         bool      `json:"empty_repo"`
	Statistics        *struct {
		RepositorySize int64 `json:"repository_size"`
	} `json:"statistics"`
}

func (p project) under(owner string) bool {
	return len(p.PathWithNamespace) > len(owner) && strings.EqualFold(p.PathWithNamespace[:len(owner)+1], owner+"/")
}

// name is the project's path below owner, its sub-groups joined by
// SubgroupSeparator.
func (p project) name(owner string) string {
	return strings.ReplaceAll(p.PathWithNamespace[len(owner)+1:], "/", SubgroupSeparator)
}

// item is the project's item without its language and its last change, which
// the listing reads apart.
func (p project) item(owner string) provider.Item {
	it := provider.Item{
		Owner:         owner,
		Name:          p.name(owner),
		Topics:        p.Topics,
		Archived:      p.Archived,
		Fork:          p.ForkedFrom != nil,
		CloneURL:      p.HTTPURLToRepo,
		DefaultBranch: p.DefaultBranch,
	}
	if p.Statistics != nil {
		it.SizeKiB = (p.Statistics.RepositorySize + 1023) / 1024
	}
	return it
}

// projectLookups bounds the requests one call has in flight: the pages of a
// listing, the projects asked for one by one, their languages and heads.
const projectLookups = 8

// perPage is a listing page's size, GitLab's largest, and the most names
// Readable asks for one by one: past it, the listing is the cheaper check.
const perPage = 100

// byActivity holds a value read for each project with the last activity it
// was read at. GitLab's listing carries neither a project's language nor its
// head, and neither moves without the project's last activity moving, so a
// listing asks for them only for projects active since their last read.
type byActivity[T any] struct {
	mu        sync.Mutex
	byProject map[int64]readAt[T]
}

type readAt[T any] struct {
	v  T
	at time.Time
	// failedAt is the last activity a read failed at, which is logged once.
	failedAt time.Time
}

func newByActivity[T any]() *byActivity[T] {
	return &byActivity[T]{byProject: map[int64]readAt[T]{}}
}

// get is p's value, current when read at p's last activity.
func (c *byActivity[T]) get(p project) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	got, ok := c.byProject[p.ID]
	return got.v, ok && got.at.Equal(p.LastActivityAt)
}

func (c *byActivity[T]) put(p project, v T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byProject[p.ID] = readAt[T]{v: v, at: p.LastActivityAt}
}

// failed records a failed read at p's last activity: the value stays the last
// one read (the zero value before the first), and stays stale, so the next
// listing asks again. It is true for the first failure at that activity.
func (c *byActivity[T]) failed(p project) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	got := c.byProject[p.ID]
	first := !got.failedAt.Equal(p.LastActivityAt)
	got.failedAt = p.LastActivityAt
	c.byProject[p.ID] = got
	return got.v, first
}

// items makes the projects' items, each with its head's committed date as
// its last change (an issue or a comment moves a project's last activity,
// only a push moves its head) and, withLanguage, its primary language: the
// one with the largest share, by name on a tie. A head that cannot be read
// leaves the item its last known date, or none, and the listing goes on; a
// language that cannot be read fails it.
func (k *Kind) items(ctx context.Context, cred oauth2.TokenSource, owner string, projects []project, withLanguage bool) ([]provider.Item, error) {
	items := make([]provider.Item, len(projects))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(projectLookups)
	for i, p := range projects {
		items[i] = p.item(owner)
		id := "/projects/" + strconv.FormatInt(p.ID, 10)
		if head, ok := k.heads.get(p); ok {
			items[i].LastChange = head
		} else {
			g.Go(func() error {
				var commit struct {
					CommittedDate time.Time `json:"committed_date"`
				}
				if err := k.get(gctx, cred, id+"/repository/commits/"+url.PathEscape(p.DefaultBranch), &commit); err != nil {
					if gctx.Err() != nil {
						return gctx.Err()
					}
					last, first := k.heads.failed(p)
					if first {
						k.log.Warn("reading a GitLab project's head; its last change stays the last one read",
							"owner", owner, "project", p.PathWithNamespace, "branch", p.DefaultBranch, "lastChange", last, "error", err)
					}
					items[i].LastChange = last
					return nil
				}
				k.heads.put(p, commit.CommittedDate)
				items[i].LastChange = commit.CommittedDate
				return nil
			})
		}
		if !withLanguage {
			continue
		}
		if lang, ok := k.langs.get(p); ok {
			items[i].Language = lang
			continue
		}
		g.Go(func() error {
			var shares map[string]float64
			if err := k.get(gctx, cred, id+"/languages", &shares); err != nil {
				return fmt.Errorf("listing %s: the languages of %s: %w", owner, p.PathWithNamespace, err)
			}
			lang := primary(shares)
			k.langs.put(p, lang)
			items[i].Language = lang
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return items, nil
}

func primary(shares map[string]float64) string {
	var best string
	for name, share := range shares {
		if best == "" || share > shares[best] || share == shares[best] && name < best {
			best = name
		}
	}
	return best
}

func (k *Kind) get(ctx context.Context, cred oauth2.TokenSource, path string, out any) error {
	_, err := k.getPage(ctx, cred, k.api.String()+path, out)
	return err
}

// getPage GETs one page and returns its header, which links the next.
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
	return k.do(req, out)
}

// A rate limit (429) is waited out as GitLab says: `Retry-After`, else the
// limit's `RateLimit-Reset`, else a minute. A wait beyond maxRateLimitWait,
// or more than rateLimitRetries of them, fails the call with the limit
// instead, for the caller's next cycle.
const (
	maxRateLimitWait = 5 * time.Minute
	rateLimitRetries = 3
)

// do sends req (which carries no body, so it is sent again after a rate
// limit's wait) and decodes a 200 answer into out.
func (k *Kind) do(req *http.Request, out any) (http.Header, error) {
	for attempt := 0; ; attempt++ {
		header, err := k.once(req, out)
		var se *statusError
		if !errors.As(err, &se) || !se.limited || attempt == rateLimitRetries || se.resetIn > maxRateLimitWait {
			return header, err
		}
		if err := k.clock.Sleep(req.Context(), se.resetIn); err != nil {
			return nil, fmt.Errorf("%w (while waiting out a rate limit: %w)", err, se)
		}
	}
}

func (k *Kind) once(req *http.Request, out any) (http.Header, error) {
	req.Header.Set("Accept", "application/json")
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		se := &statusError{method: req.Method, path: req.URL.EscapedPath(), status: resp.StatusCode, message: errorMessage(resp.Body)}
		se.resetIn, se.limited = rateLimitWait(resp, k.clock.Now())
		return nil, se
	}
	return resp.Header, json.NewDecoder(resp.Body).Decode(out)
}

// errorMessage reads GitLab's error answer: `{"message": …}` (a string or an
// object of field errors) or `{"error": …}`.
func errorMessage(body io.Reader) string {
	var e struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&e)
	var s string
	if json.Unmarshal(e.Message, &s) == nil && s != "" {
		return s
	}
	if len(e.Message) > 0 {
		return string(e.Message)
	}
	return e.Error
}

// rateLimitWait reads a rate-limited answer's wait: false for any other
// answer.
func rateLimitWait(resp *http.Response, now time.Time) (time.Duration, bool) {
	if resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
		return time.Duration(s) * time.Second, true
	}
	if reset, err := strconv.ParseInt(resp.Header.Get("RateLimit-Reset"), 10, 64); err == nil {
		// A second past the reset, so the retry lands after it.
		return max(time.Unix(reset, 0).Sub(now), 0) + time.Second, true
	}
	return time.Minute, true
}

// lastLink parses rel="last" from a Link header and its page number; zero
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

// link extracts the URL of rel from a Link header.
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

// statusError is an API answer other than 200; a rate limit says when it
// resets.
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
