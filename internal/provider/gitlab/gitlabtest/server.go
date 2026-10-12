// Package gitlabtest is a stub of the GitLab REST API's parts the gitlab kind
// calls: a group's projects with its sub-groups' (paged by a Link header), a
// user namespace's, a project's languages, with the rate-limit answers GitLab
// gives. The sync's group access token reads every project, with its
// statistics; a person's token reads the projects issued to it, and a group
// the person reads nothing of is a 404, as a private group is. An item named
// `team~api` under `acme` is the project `acme/team/api`. It goes by a clock
// the kind under test shares, moved past the listing's freshness by every
// change, as time passing would.
package gitlabtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
)

// SyncToken is the group access token the stub accepts.
const SyncToken = "glpat-stub-sync"

// DefaultPageSize is the stub's page size, small so listings page.
const DefaultPageSize = 2

var (
	tokenRef        = provider.SecretRef{Name: "gitlab-sync", Key: "token"}
	clientSecretRef = provider.SecretRef{Name: "gitlab-oauth", Key: "client-secret"}
)

// Limit is a rate-limited answer (429) the stub gives.
type Limit struct {
	// Header carries `Retry-After`, or `RateLimit-Reset`, or nothing.
	Header http.Header
}

// Server is the stub. Owners are groups unless made users.
type Server struct {
	*httptest.Server
	// Clock is the stub's time: the kind under test goes by it too.
	Clock *providertest.Clock
	// PageSize is how many projects a listing page holds.
	PageSize int

	mu        sync.Mutex
	projects  map[string]map[string]project // owner → item name → project
	users     map[string]bool
	persons   map[string]map[string]bool // person's token → owner/name they read
	ids       int64
	listed    int
	langReads int
	requests  int
	limits    []Limit
}

type project struct {
	provider.Item
	id    int64
	empty bool
}

// NewServer starts a stub, stopped when t ends.
func NewServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		Clock:    providertest.NewClock(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
		PageSize: DefaultPageSize,
		projects: map[string]map[string]project{}, users: map[string]bool{}, persons: map[string]map[string]bool{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Put creates or replaces a project, keeping its ID, and moves the clock
// past the listing's freshness so the next listing sees it.
func (s *Server) Put(it provider.Item) { s.put(it, false) }

// PutEmpty creates a project without a repository.
func (s *Server) PutEmpty(owner, name string) {
	s.put(provider.Item{Owner: owner, Name: name, LastChange: s.Clock.Now()}, true)
}

func (s *Server) put(it provider.Item, empty bool) {
	s.mu.Lock()
	if s.projects[it.Owner] == nil {
		s.projects[it.Owner] = map[string]project{}
	}
	p, ok := s.projects[it.Owner][it.Name]
	if !ok {
		s.ids++
		p.id = s.ids
	}
	p.Item, p.empty = it, empty
	p.Topics = append([]string(nil), it.Topics...)
	s.projects[it.Owner][it.Name] = p
	s.mu.Unlock()
	s.Clock.Advance(provider.ListingFreshness)
}

// Delete removes a project, and moves the clock past the listing's
// freshness so the next listing sees it gone.
func (s *Server) Delete(owner, name string) {
	s.mu.Lock()
	delete(s.projects[owner], name)
	s.mu.Unlock()
	s.Clock.Advance(provider.ListingFreshness)
}

// User makes owner a user namespace instead of a group.
func (s *Server) User(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[owner] = true
}

// Person issues a person's OAuth token reading the projects named
// `<owner>/<name>`, and no other.
func (s *Server) Person(readable ...string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tok := fmt.Sprintf("gloas-stub%d", len(s.persons)+1)
	s.persons[tok] = map[string]bool{}
	for _, key := range readable {
		s.persons[tok][key] = true
	}
	return tok
}

// LimitNext answers the next n API requests with the rate limit l.
func (s *Server) LimitNext(n int, l Limit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		s.limits = append(s.limits, l)
	}
}

// Listed is the number of listing pages served.
func (s *Server) Listed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listed
}

// LanguageReads is the number of project language requests served.
func (s *Server) LanguageReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.langReads
}

// Requests is the number of API requests received, rate-limited ones
// included.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Values configure a gitlab instance against the stub.
func (s *Server) Values() json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"url":   s.URL,
		"token": tokenRef,
		"oauth": map[string]any{"clientID": "gitlab-stub-app", "clientSecret": clientSecretRef},
	})
	return raw
}

// Secrets hold the group access token.
func (s *Server) Secrets() provider.Secrets {
	return providertest.Secrets{tokenRef: []byte(SyncToken + "\n")}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, "/api/v4/")
	if !ok || r.Method != http.MethodGet {
		fail(w, http.StatusNotFound, "404 Not Found")
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
		fail(w, http.StatusTooManyRequests, "Retry later")
		return
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	reads, person := s.persons[tok]
	s.mu.Unlock()
	if tok != SyncToken && !person {
		fail(w, http.StatusUnauthorized, "401 Unauthorized")
		return
	}

	parts := strings.Split(path, "/")
	switch {
	case len(parts) == 3 && (parts[0] == "groups" || parts[0] == "users") && parts[2] == "projects":
		owner := parts[1]
		s.mu.Lock()
		_, known := s.projects[owner]
		isUser := s.users[owner]
		s.mu.Unlock()
		if !known || isUser != (parts[0] == "users") {
			fail(w, http.StatusNotFound, "404 Group Not Found")
			return
		}
		if parts[0] == "groups" && r.URL.Query().Get("include_subgroups") != "true" {
			fail(w, http.StatusBadRequest, "the kind lists sub-groups")
			return
		}
		all := s.list(owner, reads)
		if person && len(all) == 0 && !isUser {
			// A private group the person reads nothing of.
			fail(w, http.StatusNotFound, "404 Group Not Found")
			return
		}
		s.page(w, r, owner, all, !person && r.URL.Query().Get("statistics") == "true")
	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "languages":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		p, ok := s.byID(id)
		if !ok || person && !reads[p.Key()] {
			fail(w, http.StatusNotFound, "404 Project Not Found")
			return
		}
		s.mu.Lock()
		s.langReads++
		s.mu.Unlock()
		shares := map[string]float64{}
		if p.Language != "" {
			shares[p.Language] = 70.5
			shares["Makefile"] = 29.5
		}
		writeJSON(w, http.StatusOK, shares)
	default:
		fail(w, http.StatusNotFound, "404 Not Found")
	}
}

// list are owner's projects by ID, only those in reads unless reads is nil.
func (s *Server) list(owner string, reads map[string]bool) []project {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []project
	for _, p := range s.projects[owner] {
		if reads == nil || reads[p.Key()] {
			all = append(all, p)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
	return all
}

func (s *Server) byID(id int64) (project, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ps := range s.projects {
		for _, p := range ps {
			if p.id == id {
				return p, true
			}
		}
	}
	return project{}, false
}

// page serves all as one page of a listing, with GitLab's Link header to the
// next.
func (s *Server) page(w http.ResponseWriter, r *http.Request, owner string, all []project, statistics bool) {
	s.mu.Lock()
	s.listed++
	s.mu.Unlock()

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
	for _, p := range all[from:to] {
		out = append(out, s.projectJSON(owner, p, statistics))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) projectJSON(owner string, p project, statistics bool) map[string]any {
	full := owner + "/" + strings.ReplaceAll(p.Name, "~", "/")
	topics := p.Topics
	if topics == nil {
		topics = []string{}
	}
	m := map[string]any{
		"id": p.id, "path": full[strings.LastIndex(full, "/")+1:], "path_with_namespace": full,
		"topics": topics, "archived": p.Archived, "empty_repo": p.empty,
		"last_activity_at": p.LastChange.UTC().Format("2006-01-02T15:04:05.000Z"),
		"http_url_to_repo": s.URL + "/" + full + ".git", "visibility": "private",
	}
	if !p.empty {
		m["default_branch"] = "main"
	}
	if p.Fork {
		m["forked_from_project"] = map[string]any{"id": 1, "path_with_namespace": "upstream/" + p.Name}
	}
	if statistics {
		m["statistics"] = map[string]any{"repository_size": p.SizeKiB * 1024, "storage_size": p.SizeKiB * 2048}
	}
	return m
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
