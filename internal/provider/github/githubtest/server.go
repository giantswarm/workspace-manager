// Package githubtest is a stub of the GitHub REST API's parts the github kind
// calls: a GitHub App's installation on an organization or a user account,
// its installation tokens, whose JWTs it verifies, and the installation's
// repository listing (paged), with the rate-limit answers GitHub gives. It
// goes by a clock the kind under test shares, moved past the listing's
// freshness by every change, as time passing would.
package githubtest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
)

// AppID is the App the stub accepts JWTs of.
const AppID = "4242"

// DefaultPageSize is the stub's page size, small so listings page.
const DefaultPageSize = 2

var (
	privateKeyRef   = provider.SecretRef{Name: "github-app", Key: "private-key"}
	clientSecretRef = provider.SecretRef{Name: "github-oauth", Key: "client-secret"}
)

// Clock is a stopped clock: it moves when advanced, and a Sleep moves it by
// the duration slept, which it records.
type Clock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

// Now implements provider.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep implements provider.Clock without waiting.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	return nil
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Slept are the waits so far.
func (c *Clock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// Limit is a rate-limited answer the stub gives.
type Limit struct {
	// Status is 403 or 429.
	Status int
	// Header carries `Retry-After`, or `X-RateLimit-Remaining` and
	// `X-RateLimit-Reset`, or nothing.
	Header http.Header
	// Message is the answer's message.
	Message string
}

// Server is the stub. Owners are organizations unless made users.
type Server struct {
	*httptest.Server
	// Clock is the stub's time: the kind under test goes by it too.
	Clock *Clock
	// PageSize is how many repositories a listing page holds.
	PageSize int
	key      *rsa.PrivateKey

	mu       sync.Mutex
	repos    map[string]map[string]provider.Item
	users    map[string]bool
	tokens   map[string]string // installation token → owner
	minted   int
	listed   int
	requests int
	limits   []Limit
}

// NewServer starts a stub, stopped when t ends.
func NewServer(t *testing.T) *Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Clock: &Clock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}, PageSize: DefaultPageSize, key: key,
		repos: map[string]map[string]provider.Item{}, users: map[string]bool{}, tokens: map[string]string{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Put creates or replaces a repository, and moves the clock past the
// listing's freshness so the next listing sees it.
func (s *Server) Put(it provider.Item) {
	s.mu.Lock()
	if s.repos[it.Owner] == nil {
		s.repos[it.Owner] = map[string]provider.Item{}
	}
	s.repos[it.Owner][it.Name] = it
	s.mu.Unlock()
	s.Clock.Advance(provider.ListingFreshness)
}

// Delete removes a repository, and moves the clock past the listing's
// freshness so the next listing sees it gone.
func (s *Server) Delete(owner, name string) {
	s.mu.Lock()
	delete(s.repos[owner], name)
	s.mu.Unlock()
	s.Clock.Advance(provider.ListingFreshness)
}

// User makes owner a user account instead of an organization.
func (s *Server) User(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[owner] = true
}

// LimitNext answers the next n API requests with the rate limit l.
func (s *Server) LimitNext(n int, l Limit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.limits = append(s.limits, l)
	}
}

// Minted is the number of installation tokens issued.
func (s *Server) Minted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.minted
}

// Listed is the number of listing pages served.
func (s *Server) Listed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listed
}

// Requests is the number of API requests received, rate-limited ones
// included.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Values configure a github instance against the stub.
func (s *Server) Values() json.RawMessage {
	v := map[string]any{
		"url":    s.URL,
		"apiURL": s.URL + "/api/v3",
		"app":    map[string]any{"id": AppID, "privateKey": privateKeyRef},
		"oauth":  map[string]any{"clientID": "Iv1.stub", "clientSecret": clientSecretRef},
	}
	raw, _ := json.Marshal(v)
	return raw
}

// Secrets hold the App's private key.
func (s *Server) Secrets() provider.Secrets {
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(s.key)})
	return providertest.Secrets{privateKeyRef: pemKey}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, "/api/v3/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.requests++
	var limit *Limit
	if len(s.limits) > 0 {
		limit, s.limits = &s.limits[0], s.limits[1:]
	}
	s.mu.Unlock()
	if limit != nil {
		for k, vs := range limit.Header {
			w.Header()[k] = vs
		}
		fail(w, limit.Status, limit.Message)
		return
	}
	parts := strings.Split(path, "/")
	switch {
	case r.Method == http.MethodGet && len(parts) == 3 && (parts[0] == "orgs" || parts[0] == "users") && parts[2] == "installation":
		if !s.validJWT(r) {
			fail(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		s.mu.Lock()
		_, known := s.repos[parts[1]]
		isUser := s.users[parts[1]]
		s.mu.Unlock()
		if !known || isUser != (parts[0] == "users") {
			fail(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": installationID(parts[1])})
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "app" && parts[1] == "installations" && parts[3] == "access_tokens":
		if !s.validJWT(r) {
			fail(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		s.mu.Lock()
		s.minted++
		tok := fmt.Sprintf("ghs_stub%d", s.minted)
		for owner := range s.repos {
			if strconv.FormatInt(installationID(owner), 10) == parts[2] {
				s.tokens[tok] = owner
			}
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": s.Clock.Now().Add(time.Hour).UTC()})
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "installation" && parts[1] == "repositories":
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		owner, ok := s.tokens[tok]
		s.mu.Unlock()
		if !ok {
			fail(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		s.listRepos(w, r, owner)
	default:
		fail(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request, owner string) {
	s.mu.Lock()
	s.listed++
	var all []provider.Item
	for _, it := range s.repos[owner] {
		all = append(all, it)
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	from, to := min((page-1)*s.PageSize, len(all)), min(page*s.PageSize, len(all))
	if to < len(all) {
		next := *r.URL
		q := next.Query()
		q.Set("page", strconv.Itoa(page+1))
		next.RawQuery = q.Encode()
		w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, s.URL, next.RequestURI()))
	}
	out := make([]map[string]any, 0, to-from)
	for _, it := range all[from:to] {
		topics := it.Topics
		if topics == nil {
			topics = []string{}
		}
		var lang any
		if it.Language != "" {
			lang = it.Language
		}
		out = append(out, map[string]any{
			"name": it.Name, "full_name": owner + "/" + it.Name, "owner": map[string]any{"login": owner},
			"language": lang, "topics": topics, "archived": it.Archived, "fork": it.Fork,
			"pushed_at": it.LastChange.UTC().Format(time.RFC3339), "size": it.SizeKiB,
			"clone_url": s.URL + "/" + owner + "/" + it.Name + ".git", "default_branch": "main",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(all), "repository_selection": "all", "repositories": out})
}

func (s *Server) validJWT(r *http.Request) bool {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return false
	}
	var c jwt.Claims
	if err := tok.Claims(&s.key.PublicKey, &c); err != nil {
		return false
	}
	if c.Expiry == nil || c.IssuedAt == nil || c.Expiry.Time().Sub(c.IssuedAt.Time()) > 10*time.Minute {
		return false
	}
	return c.Validate(jwt.Expected{Issuer: AppID, Time: s.Clock.Now()}) == nil
}

func installationID(owner string) int64 {
	var h int64 = 7
	for _, c := range owner {
		h = h*31 + int64(c)
	}
	return h & 0xffffff
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
