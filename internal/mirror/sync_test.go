package mirror

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMain lets git run this test binary as the credential helper: the sync
// under test configures `<test binary> <credential dir>` as git's helper, and
// git appends the operation.
func TestMain(m *testing.M) {
	if os.Getenv("WORKSPACE_MANAGER_TEST_CREDENTIAL_HELPER") == "1" && len(os.Args) == 3 {
		if err := ServeCredential(os.Args[1], os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var (
	t0 = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
)

type harness struct {
	t        *testing.T
	upstream *gitServer
	volume   string
	logs     *bytes.Buffer
	syncer   *Syncer
}

// newHarness wires a sync against the fixture: a credential directory for
// the provider "github" holding the fixture's token, and this test binary as
// the credential helper.
func newHarness(t *testing.T) *harness {
	t.Helper()
	upstream := newGitServer(t)
	creds := t.TempDir()
	dir := filepath.Join(creds, "github")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, credentialUsernameFile), []byte(fixtureUsername+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, credentialTokenFile), []byte(upstream.token+"\n"), 0o600))
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("WORKSPACE_MANAGER_TEST_CREDENTIAL_HELPER", "1")
	logs := &bytes.Buffer{}
	h := &harness{t: t, upstream: upstream, volume: t.TempDir(), logs: logs}
	h.syncer = &Syncer{
		Volume:           h.volume,
		CredentialsDir:   creds,
		CredentialHelper: []string{exe},
		Parallel:         2,
		Log:              slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	return h
}

func (h *harness) repo(owner, name string, pushedAt time.Time) Repository {
	return Repository{Provider: "github", Owner: owner, Name: name, CloneURL: h.upstream.url(owner, name),
		DefaultBranch: "main", PushedAt: pushedAt}
}

func (h *harness) sync(repos ...Repository) *Result {
	h.t.Helper()
	res, err := h.syncer.Run(context.Background(), &Selection{Repositories: repos})
	require.NoError(h.t, err)
	return res
}

func (h *harness) mirror(owner, name string) string {
	return filepath.Join(h.volume, MirrorsDir, owner, name+".git")
}

func (h *harness) manifest() *Manifest {
	h.t.Helper()
	m, err := ReadManifest(h.volume)
	require.NoError(h.t, err)
	return m
}

// assertNoToken greps the volume, every file of it, and the sync's logs for
// the fixture's token.
func (h *harness) assertNoToken() {
	h.t.Helper()
	token := []byte(h.upstream.token)
	err := filepath.WalkDir(h.volume, func(p string, d fs.DirEntry, err error) error {
		require.NoError(h.t, err)
		if !d.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(p) //nolint:gosec // every file of the test's own volume
		require.NoError(h.t, err)
		require.False(h.t, bytes.Contains(raw, token), "token found on the volume in %s", p)
		return nil
	})
	require.NoError(h.t, err)
	require.NotContains(h.t, h.logs.String(), h.upstream.token, "token found in the logs")
}

func TestSyncAddsFetchesAndDrops(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	alpha1 := h.upstream.commit("acme", "alpha", "README.md", "alpha")
	h.upstream.create("acme", "beta")
	beta1 := h.upstream.commit("acme", "beta", "README.md", "beta")

	// Add: both repositories are new.
	res := h.sync(h.repo("acme", "alpha", t0), h.repo("acme", "beta", t0))
	require.Equal(t, []string{"acme/alpha", "acme/beta"}, res.Added)
	require.Empty(t, res.Fetched)
	require.DirExists(t, h.mirror("acme", "alpha"))
	require.Equal(t, "true", runGit(t, h.mirror("acme", "alpha"), "rev-parse", "--is-bare-repository"))
	require.Equal(t, "never", runGit(t, h.mirror("acme", "alpha"), "config", "gc.pruneExpire"))
	m := h.manifest()
	require.Len(t, m.Repositories, 2)
	require.Equal(t, alpha1, m.Entry("acme", "alpha").Head)
	require.Equal(t, beta1, m.Entry("acme", "beta").Head)
	require.Equal(t, "mirrors/acme/alpha.git", m.Entry("acme", "alpha").Path)
	require.Positive(t, m.Entry("acme", "alpha").SizeBytes)
	require.Nil(t, m.Entry("acme", "alpha").DroppedAt)
	require.Positive(t, h.upstream.requestsFor("acme", "alpha"))
	require.Positive(t, h.upstream.requestsFor("acme", "beta"))

	// Change: alpha was pushed to, beta was not. Only alpha is fetched; the
	// fixture sees no request for beta.
	h.upstream.resetRequests()
	alpha2 := h.upstream.commit("acme", "alpha", "second.txt", "second")
	res = h.sync(h.repo("acme", "alpha", t1), h.repo("acme", "beta", t0))
	require.Equal(t, []string{"acme/alpha"}, res.Fetched)
	require.Equal(t, []string{"acme/beta"}, res.Unchanged)
	require.Empty(t, res.Added)
	m = h.manifest()
	require.Equal(t, alpha2, m.Entry("acme", "alpha").Head)
	require.Equal(t, t1, m.Entry("acme", "alpha").PushedAt)
	require.Equal(t, beta1, m.Entry("acme", "beta").Head)
	require.Positive(t, h.upstream.requestsFor("acme", "alpha"))
	require.Zero(t, h.upstream.requestsFor("acme", "beta"), "an unchanged repository is not contacted")

	// Drop: beta leaves the selection. Its mirror stays, marked; nothing is
	// contacted.
	h.upstream.resetRequests()
	res = h.sync(h.repo("acme", "alpha", t1))
	require.Equal(t, []string{"acme/beta"}, res.Dropped)
	require.Equal(t, []string{"acme/alpha"}, res.Unchanged)
	m = h.manifest()
	require.NotNil(t, m.Entry("acme", "beta").DroppedAt)
	require.Equal(t, beta1, m.Entry("acme", "beta").Head)
	require.DirExists(t, h.mirror("acme", "beta"))
	require.Zero(t, h.upstream.requestsFor("acme", "alpha"))
	require.Zero(t, h.upstream.requestsFor("acme", "beta"))

	// Back: beta is selected again at its old push time; the mark goes
	// without a fetch.
	res = h.sync(h.repo("acme", "alpha", t1), h.repo("acme", "beta", t0))
	require.Equal(t, []string{"acme/alpha", "acme/beta"}, res.Unchanged)
	require.Nil(t, h.manifest().Entry("acme", "beta").DroppedAt)
	require.Zero(t, h.upstream.requestsFor("acme", "beta"))

	// No token on the volume, in a mirror's configuration or in the logs.
	config, err := os.ReadFile(filepath.Join(h.mirror("acme", "alpha"), "config"))
	require.NoError(t, err)
	require.NotContains(t, string(config), h.upstream.token)
	require.NotContains(t, string(config), "credential")
	h.assertNoToken()
}

func TestSyncKeepsObjectsBorrowedBySessions(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	c1 := h.upstream.commit("acme", "alpha", "one.txt", "one")
	c2 := h.upstream.commit("acme", "alpha", "two.txt", "two")
	h.sync(h.repo("acme", "alpha", t0))

	// A Session's clone borrows the mirror's objects (git alternates) before
	// the force-push.
	session := filepath.Join(h.volume, SessionsDir, "s1", "alpha")
	require.NoError(t, os.MkdirAll(filepath.Dir(session), 0o700))
	runGit(t, "", "clone", "--quiet", "--shared", "--no-checkout", h.mirror("acme", "alpha"), session)
	require.Equal(t, c2, runGit(t, session, "rev-parse", "HEAD"))

	// Upstream rewinds main below c2 and force-pushes c3: c2 is unreachable
	// upstream and, after the fetch, in the mirror.
	c3 := h.upstream.forcePush("acme", "alpha", c1, "three.txt", "three")
	res := h.sync(h.repo("acme", "alpha", t1))
	require.Equal(t, []string{"acme/alpha"}, res.Fetched)
	require.Equal(t, c3, h.manifest().Entry("acme", "alpha").Head)
	require.Equal(t, c3, runGit(t, h.mirror("acme", "alpha"), "rev-parse", "HEAD"))
	require.Contains(t, h.logs.String(), "keeping unreachable objects")

	// The gc kept c2 for the borrowing clone: its fsck reads every object.
	runGit(t, session, "fsck", "--full", "--strict")
	require.Equal(t, c2, runGit(t, session, "rev-parse", "HEAD"))
	runGit(t, session, "cat-file", "-e", c2+"^{commit}")
	runGit(t, h.mirror("acme", "alpha"), "cat-file", "-e", c2+"^{commit}")
	require.Equal(t, "never", runGit(t, h.mirror("acme", "alpha"), "config", "gc.pruneExpire"))
	h.assertNoToken()
}

func TestSyncPrunesUnborrowedObjects(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	c1 := h.upstream.commit("acme", "alpha", "one.txt", "one")
	c2 := h.upstream.commit("acme", "alpha", "two.txt", "two")
	h.sync(h.repo("acme", "alpha", t0))

	// With no session directory borrowing from the mirror, the force-pushed
	// away commit is pruned; the mirror stays consistent.
	h.upstream.forcePush("acme", "alpha", c1, "three.txt", "three")
	h.sync(h.repo("acme", "alpha", t1))
	require.Contains(t, h.logs.String(), "pruning unreachable objects")
	require.True(t, gitFails(t, h.mirror("acme", "alpha"), "cat-file", "-e", c2+"^{commit}"), "c2 should be pruned")
	runGit(t, h.mirror("acme", "alpha"), "fsck", "--full", "--strict")
}

func TestSyncFollowsAMovedDefaultBranch(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	c1 := h.upstream.commit("acme", "alpha", "one.txt", "one")
	w := h.upstream.workClone("acme", "alpha")
	runGit(t, w, "switch", "--quiet", "-c", "develop")
	require.NoError(t, os.WriteFile(filepath.Join(w, "dev.txt"), []byte("dev"), 0o600))
	runGit(t, w, "add", "dev.txt")
	runGit(t, w, "commit", "--quiet", "-m", "dev")
	runGit(t, w, "push", "--quiet", "origin", "develop")
	c2 := runGit(t, w, "rev-parse", "HEAD")

	h.sync(h.repo("acme", "alpha", t1))
	require.Equal(t, c1, h.manifest().Entry("acme", "alpha").Head)

	// The provider's default branch changes without a push: HEAD and the
	// manifest follow locally, nothing is contacted.
	h.upstream.resetRequests()
	r := h.repo("acme", "alpha", t1)
	r.DefaultBranch = "develop"
	res := h.sync(r)
	require.Equal(t, []string{"acme/alpha"}, res.Unchanged)
	require.Equal(t, c2, h.manifest().Entry("acme", "alpha").Head)
	require.Equal(t, "refs/heads/develop", runGit(t, h.mirror("acme", "alpha"), "symbolic-ref", "HEAD"))
	require.Zero(t, h.upstream.requestsFor("acme", "alpha"))
}

func TestSyncFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	h.upstream.commit("acme", "alpha", "README.md", "alpha")
	h.sync(h.repo("acme", "alpha", t0))
	before, err := os.ReadFile(filepath.Join(h.volume, ManifestFile))
	require.NoError(t, err)

	// A repository the server does not have fails the run: the manifest is
	// untouched and no partial mirror remains.
	_, err = h.syncer.Run(context.Background(), &Selection{Repositories: []Repository{
		h.repo("acme", "alpha", t0), h.repo("acme", "missing", t0)}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "acme/missing")
	after, err := os.ReadFile(filepath.Join(h.volume, ManifestFile))
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	require.NoDirExists(t, h.mirror("acme", "missing"))
	entries, err := os.ReadDir(filepath.Join(h.volume, MirrorsDir, "acme"))
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasPrefix(e.Name(), tmpPrefix), "leftover %s", e.Name())
	}
	h.assertNoToken()
}

func TestSyncRemovesAnInterruptedClone(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	h.upstream.commit("acme", "alpha", "README.md", "alpha")
	leftover := filepath.Join(h.volume, MirrorsDir, "acme", tmpPrefix+"alpha.git")
	require.NoError(t, os.MkdirAll(leftover, 0o700))

	res := h.sync(h.repo("acme", "alpha", t0))
	require.Equal(t, []string{"acme/alpha"}, res.Added)
	require.NoDirExists(t, leftover)
}

func TestSyncRecordsAMirrorTheManifestNeverSaw(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	h.upstream.commit("acme", "alpha", "README.md", "alpha")
	h.upstream.create("acme", "beta")
	beta1 := h.upstream.commit("acme", "beta", "README.md", "beta")
	h.sync(h.repo("acme", "alpha", t0), h.repo("acme", "beta", t0))

	// A sync interrupted between the clone and the manifest leaves a mirror
	// the manifest does not know; once unselected it is recorded as dropped.
	require.NoError(t, (&Manifest{}).Write(h.volume))
	res := h.sync(h.repo("acme", "alpha", t0))
	require.Equal(t, []string{"acme/alpha"}, res.Fetched, "a mirror unknown to the manifest is fetched")
	require.Equal(t, []string{"acme/beta"}, res.Dropped)
	e := h.manifest().Entry("acme", "beta")
	require.NotNil(t, e.DroppedAt)
	require.Equal(t, beta1, e.Head)
	require.Positive(t, e.SizeBytes)
}

func TestSyncRefusesACredentialInTheURL(t *testing.T) {
	h := newHarness(t)
	r := h.repo("acme", "alpha", t0)
	r.CloneURL = strings.Replace(r.CloneURL, "http://", "http://user:"+h.upstream.token+"@", 1)
	_, err := h.syncer.Run(context.Background(), &Selection{Repositories: []Repository{r}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "carries a credential")
	require.Zero(t, h.upstream.requestsFor("acme", "alpha"))
}

func TestSyncAnonymousWithoutACredentialDirectory(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	h.upstream.commit("acme", "alpha", "README.md", "alpha")
	r := h.repo("acme", "alpha", t0)
	r.Provider = "public"

	// The fixture wants its token, so an anonymous clone fails, and it fails
	// closed; the log names the provider, not a credential.
	_, err := h.syncer.Run(context.Background(), &Selection{Repositories: []Repository{r}})
	require.Error(t, err)
	require.Contains(t, h.logs.String(), "fetching anonymously")
	h.assertNoToken()
}

func TestSyncReportsTheRemovedMirror(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	h.upstream.commit("acme", "alpha", "README.md", "alpha")
	h.upstream.create("acme", "beta")
	h.upstream.commit("acme", "beta", "README.md", "beta")
	h.sync(h.repo("acme", "alpha", t0), h.repo("acme", "beta", t0))
	h.sync(h.repo("acme", "alpha", t0))
	require.NotNil(t, h.manifest().Entry("acme", "beta").DroppedAt)

	// The cleanup removed the dropped mirror: the entry leaves the manifest.
	require.NoError(t, os.RemoveAll(h.mirror("acme", "beta")))
	res := h.sync(h.repo("acme", "alpha", t0))
	require.Equal(t, []string{"acme/beta"}, res.Removed)
	require.Nil(t, h.manifest().Entry("acme", "beta"))
	require.Len(t, h.manifest().Repositories, 1)
	_ = t2
}
