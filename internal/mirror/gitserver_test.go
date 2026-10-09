package mirror

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fixtureUsername is the username the fixture's token belongs to, as a
// GitHub App installation token's is x-access-token.
const fixtureUsername = "x-access-token"

// gitServer is a local git server over HTTP: git's smart protocol for
// fetching (what git http-backend serves, spoken here over `git upload-pack`
// since a minimal git lacks http-backend) behind a token check, recording
// every request per repository, with the upstream repositories as bare
// repositories on disk and a work clone each to push from.
type gitServer struct {
	t      *testing.T
	root   string
	work   string
	token  string
	server *httptest.Server

	mu       sync.Mutex
	requests map[string]int
}

func newGitServer(t *testing.T) *gitServer {
	t.Helper()
	_, err := exec.LookPath("git")
	require.NoError(t, err, "the fixture needs git")
	var raw [16]byte
	_, err = rand.Read(raw[:])
	require.NoError(t, err)
	s := &gitServer{t: t, root: t.TempDir(), work: t.TempDir(), token: "fixture-token-" + hex.EncodeToString(raw[:]), requests: map[string]int{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r.URL.Path)
		user, pass, ok := r.BasicAuth()
		if !ok || user != fixtureUsername || pass != s.token {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.serve(w, r)
	}))
	t.Cleanup(s.server.Close)
	return s
}

// record counts a request against its repository, /<owner>/<name>.git/...
func (s *gitServer) record(urlPath string) {
	parts := strings.SplitN(strings.TrimPrefix(urlPath, "/"), "/", 3)
	if len(parts) < 2 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[parts[0]+"/"+strings.TrimSuffix(parts[1], ".git")]++
}

// serve answers the two requests of a smart-HTTP fetch: the reference
// advertisement (GET <repo>/info/refs?service=git-upload-pack) and the
// upload-pack exchange (POST <repo>/git-upload-pack), each run through
// `git upload-pack --stateless-rpc` on the bare repository, with the
// client's protocol version passed along.
func (s *gitServer) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	dir := filepath.Join(s.root, parts[0], parts[1])
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		http.NotFound(w, r)
		return
	}
	env := append(os.Environ(), "GIT_PROTOCOL="+r.Header.Get("Git-Protocol"))
	switch {
	case r.Method == http.MethodGet && parts[2] == "info/refs" && r.URL.Query().Get("service") == "git-upload-pack":
		cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", "--advertise-refs", dir) //nolint:gosec // the fixture's own repository
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		service := "# service=git-upload-pack\n"
		_, _ = fmt.Fprintf(w, "%04x%s0000", len(service)+4, service)
		_, _ = w.Write(out)
	case r.Method == http.MethodPost && parts[2] == "git-upload-pack":
		body := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer gz.Close()
			body = gz
		}
		cmd := exec.CommandContext(r.Context(), "git", "upload-pack", "--stateless-rpc", dir) //nolint:gosec // the fixture's own repository
		cmd.Env = env
		cmd.Stdin = body
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		cmd.Stdout = w
		if err := cmd.Run(); err != nil {
			s.t.Logf("upload-pack %s: %v", dir, err)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *gitServer) requestsFor(owner, name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[owner+"/"+name]
}

func (s *gitServer) resetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = map[string]int{}
}

func (s *gitServer) url(owner, name string) string {
	return s.server.URL + "/" + owner + "/" + name + ".git"
}

func (s *gitServer) bare(owner, name string) string {
	return filepath.Join(s.root, owner, name+".git")
}

func (s *gitServer) workClone(owner, name string) string {
	return filepath.Join(s.work, owner, name)
}

// create makes an empty upstream repository with main as its default branch
// and a work clone to push from.
func (s *gitServer) create(owner, name string) {
	s.t.Helper()
	require.NoError(s.t, os.MkdirAll(filepath.Dir(s.bare(owner, name)), 0o700))
	s.git("", "init", "--bare", "--quiet", "-b", "main", s.bare(owner, name))
	require.NoError(s.t, os.MkdirAll(filepath.Dir(s.workClone(owner, name)), 0o700))
	s.git("", "clone", "--quiet", s.bare(owner, name), s.workClone(owner, name))
	s.git(s.workClone(owner, name), "switch", "--quiet", "-c", "main")
}

// commit adds a file in the work clone, commits and pushes; it returns the
// commit.
func (s *gitServer) commit(owner, name, file, content string) string {
	s.t.Helper()
	w := s.workClone(owner, name)
	require.NoError(s.t, os.WriteFile(filepath.Join(w, file), []byte(content), 0o600))
	s.git(w, "add", file)
	s.git(w, "commit", "--quiet", "-m", "add "+file)
	s.git(w, "push", "--quiet", "origin", "main")
	return s.git(w, "rev-parse", "HEAD")
}

// forcePush rewinds main to the given commit, commits a file on top and
// force-pushes, leaving the commits in between unreachable upstream.
func (s *gitServer) forcePush(owner, name, backTo, file, content string) string {
	s.t.Helper()
	w := s.workClone(owner, name)
	s.git(w, "reset", "--hard", "--quiet", backTo)
	require.NoError(s.t, os.WriteFile(filepath.Join(w, file), []byte(content), 0o600))
	s.git(w, "add", file)
	s.git(w, "commit", "--quiet", "-m", "add "+file)
	s.git(w, "push", "--quiet", "--force", "origin", "main")
	return s.git(w, "rev-parse", "HEAD")
}

// git runs git for the fixture and returns its trimmed stdout.
func (s *gitServer) git(dir string, args ...string) string {
	s.t.Helper()
	return runGit(s.t, dir, args...)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // the fixture's own git calls
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.com")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// gitFails runs git and returns whether it failed.
func gitFails(t *testing.T, dir string, args ...string) bool {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // the fixture's own git calls
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	return cmd.Run() != nil
}
