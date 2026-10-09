// Package githubtest is a stub of the GitHub REST API's parts the github kind
// calls: accounts, repository listings (paged), and a GitHub App's
// installation tokens, whose JWTs it verifies.
package githubtest

import (
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

// PageSize is the stub's page size, small so listings page.
const PageSize = 2

var (
	privateKeyRef   = provider.SecretRef{Name: "github-app", Key: "private-key"}
	clientSecretRef = provider.SecretRef{Name: "github-oauth", Key: "client-secret"}
)

// Server is the stub. Owners are organizations unless made users.
type Server struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	repos  map[string]map[string]provider.Item
	users  map[string]bool
	tokens map[string]string // installation token → owner
	minted int
}

// NewServer starts a stub, stopped when t ends.
func NewServer(t *testing.T) *Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{key: key, repos: map[string]map[string]provider.Item{}, users: map[string]bool{}, tokens: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Put creates or replaces a repository.
func (s *Server) Put(it provider.Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repos[it.Owner] == nil {
		s.repos[it.Owner] = map[string]provider.Item{}
	}
	s.repos[it.Owner][it.Name] = it
}

// Delete removes a repository.
func (s *Server) Delete(owner, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.repos[owner], name)
}

// User makes owner a user account instead of an organization.
func (s *Server) User(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[owner] = true
}

// Minted is the number of installation tokens issued.
func (s *Server) Minted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.minted
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
	parts := strings.Split(path, "/")
	switch {
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "users" && parts[2] == "installation":
		if !s.validJWT(r) {
			fail(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		s.mu.Lock()
		_, known := s.repos[parts[1]]
		s.mu.Unlock()
		if !known {
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
		writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": time.Now().Add(time.Hour).UTC()})
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "users":
		owner := s.authorized(w, r, parts[1])
		if owner == "" {
			return
		}
		typ := "Organization"
		s.mu.Lock()
		if s.users[owner] {
			typ = "User"
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"login": owner, "type": typ})
	case r.Method == http.MethodGet && len(parts) == 3 && parts[2] == "repos" && (parts[0] == "orgs" || parts[0] == "users"):
		owner := s.authorized(w, r, parts[1])
		if owner == "" {
			return
		}
		s.mu.Lock()
		isUser := s.users[owner]
		s.mu.Unlock()
		if isUser != (parts[0] == "users") {
			fail(w, http.StatusNotFound, "Not Found")
			return
		}
		s.listRepos(w, r, owner)
	default:
		fail(w, http.StatusNotFound, "Not Found")
	}
}

// authorized checks the bearer is an installation token on owner.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request, owner string) string {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens[tok] != owner {
		fail(w, http.StatusUnauthorized, "Bad credentials")
		return ""
	}
	return owner
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request, owner string) {
	s.mu.Lock()
	var all []provider.Item
	for _, it := range s.repos[owner] {
		all = append(all, it)
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	from, to := min((page-1)*PageSize, len(all)), min(page*PageSize, len(all))
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
			"name": it.Name, "full_name": owner + "/" + it.Name, "language": lang, "topics": topics,
			"archived": it.Archived, "fork": it.Fork, "pushed_at": it.LastChange.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, out)
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
	return c.Validate(jwt.Expected{Issuer: AppID, Time: time.Now()}) == nil
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
