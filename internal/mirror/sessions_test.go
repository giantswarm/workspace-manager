package mirror

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// window is the tests' short cleanup window.
const window = time.Hour

// run syncs sel at the clock now.
func (h *harness) run(now time.Time, sel *Selection) *Result {
	h.t.Helper()
	h.syncer.Now = func() time.Time { return now }
	res, err := h.syncer.Run(context.Background(), sel)
	require.NoError(h.t, err)
	return res
}

// sessionDir makes a session directory holding one file of size bytes.
func (h *harness) sessionDir(id string, size int) string {
	h.t.Helper()
	dir := filepath.Join(h.volume, SessionsDir, id)
	require.NoError(h.t, os.MkdirAll(dir, 0o700))
	require.NoError(h.t, os.WriteFile(filepath.Join(dir, "work.txt"), make([]byte, size), 0o600))
	return dir
}

func (h *harness) inventory() map[string]SessionDirectory {
	h.t.Helper()
	out := map[string]SessionDirectory{}
	for _, d := range h.manifest().Sessions {
		out[d.Session] = d
	}
	return out
}

func cleanup(sessions map[string]Session) *Cleanup {
	if sessions == nil {
		sessions = map[string]Session{}
	}
	return &Cleanup{AfterSeconds: int64(window / time.Second), Sessions: sessions}
}

func TestCleanupDeletesAnOldDirectoryAndKeepsANewer(t *testing.T) {
	h := newHarness(t)
	old := h.sessionDir("old", 10)
	newer := h.sessionDir("newer", 2048)
	now := t2
	res := h.run(now, &Selection{Cleanup: cleanup(map[string]Session{
		"old":   {Name: "Review PR", Creator: "ana@example.com", LastTurn: now.Add(-window - time.Minute)},
		"newer": {Name: "Fix the flaky test", Creator: "you@example.com", CreatedAt: now.Add(-2 * window), LastTurn: now.Add(-10 * time.Minute)},
	})})

	require.Equal(t, []string{"old"}, res.CleanedUp)
	require.NoDirExists(t, old)
	require.DirExists(t, newer)
	inv := h.inventory()
	require.Len(t, inv, 1)
	d := inv["newer"]
	require.True(t, d.Known)
	require.Equal(t, "Fix the flaky test", d.Name)
	require.Equal(t, "you@example.com", d.Creator)
	require.Equal(t, now.Add(-10*time.Minute), *d.LastTurn)
	require.Equal(t, now.Add(-10*time.Minute+window), d.CleanupAfter, "the window runs from the last turn")
	require.Equal(t, int64(2048), d.SizeBytes)
	require.Contains(t, h.logs.String(), "the Session's last turn is past the cleanup window")

	// A turn moves the date: the directory outlives its old cleanup date.
	later := d.CleanupAfter.Add(time.Minute)
	h.run(later, &Selection{Cleanup: cleanup(map[string]Session{"newer": {LastTurn: later.Add(-time.Minute)}})})
	require.DirExists(t, newer)
	require.Equal(t, later.Add(-time.Minute+window), h.inventory()["newer"].CleanupAfter)
}

func TestCleanupCountsFromCreationBeforeTheFirstTurn(t *testing.T) {
	h := newHarness(t)
	dir := h.sessionDir("idle", 1)
	created := t0
	h.run(created.Add(window-time.Second), &Selection{Cleanup: cleanup(map[string]Session{"idle": {CreatedAt: created}})})
	require.DirExists(t, dir)
	require.Nil(t, h.inventory()["idle"].LastTurn)
	require.Equal(t, created.Add(window), h.inventory()["idle"].CleanupAfter)

	h.run(created.Add(window), &Selection{Cleanup: cleanup(map[string]Session{"idle": {CreatedAt: created}})})
	require.NoDirExists(t, dir)
}

func TestCleanupDeletesTheDirectoryOfADeletedSession(t *testing.T) {
	h := newHarness(t)
	dir := h.sessionDir("s1", 1)
	h.run(t0, &Selection{Cleanup: cleanup(map[string]Session{"s1": {LastTurn: t0}})})
	require.DirExists(t, dir)
	require.True(t, h.inventory()["s1"].Known)

	// kagent no longer lists it: deleted, its directory goes at once, long
	// before its window.
	res := h.run(t0.Add(time.Minute), &Selection{Cleanup: cleanup(nil)})
	require.Equal(t, []string{"s1"}, res.CleanedUp)
	require.NoDirExists(t, dir)
	require.Empty(t, h.manifest().Sessions)
	require.Contains(t, h.logs.String(), "kagent no longer lists the Session")
}

func TestCleanupUnknownDirectoryGoesAfterTheWindowFromFirstSight(t *testing.T) {
	h := newHarness(t)
	dir := h.sessionDir("stray", 1)
	h.run(t0, &Selection{Cleanup: cleanup(nil)})
	d := h.inventory()["stray"]
	require.False(t, d.Known)
	require.Equal(t, t0, d.FirstSeen)
	require.Equal(t, t0.Add(window), d.CleanupAfter)

	// Never listed, so never "deleted at once": it waits for its window.
	h.run(t0.Add(window-time.Second), &Selection{Cleanup: cleanup(nil)})
	require.DirExists(t, dir)
	require.Equal(t, t0, h.inventory()["stray"].FirstSeen)

	res := h.run(t0.Add(window), &Selection{Cleanup: cleanup(nil)})
	require.Equal(t, []string{"stray"}, res.CleanedUp)
	require.NoDirExists(t, dir)
}

func TestWithoutACleanupNothingIsDeleted(t *testing.T) {
	h := newHarness(t)
	dir := h.sessionDir("s1", 5)
	h.run(t0, &Selection{Cleanup: cleanup(map[string]Session{"s1": {Name: "kept", LastTurn: t0}})})

	// kagent could not be read: nothing goes, long past the window too, and
	// what the last listing said stands.
	res := h.run(t0.Add(10*window), &Selection{})
	require.Empty(t, res.CleanedUp)
	require.DirExists(t, dir)
	d := h.inventory()["s1"]
	require.True(t, d.Known)
	require.Equal(t, "kept", d.Name)
	require.Equal(t, t0.Add(window), d.CleanupAfter)
	require.Equal(t, int64(5), d.SizeBytes)
}

func TestCleanupRemovesAnInterruptedDeletion(t *testing.T) {
	h := newHarness(t)
	leftover := h.sessionDir(deletedPrefix+"s1", 1)
	h.run(t0, &Selection{Cleanup: cleanup(nil)})
	require.NoDirExists(t, leftover)
	require.Empty(t, h.manifest().Sessions)
}

func TestADroppedMirrorGoesWithItsLastBorrower(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	h.upstream.commit("acme", "alpha", "README.md", "alpha")
	h.upstream.create("acme", "beta")
	h.upstream.commit("acme", "beta", "README.md", "beta")
	h.run(t0, &Selection{Repositories: []Repository{h.repo("acme", "alpha", t0), h.repo("acme", "beta", t0)}})
	h.borrow("s1", "acme", "beta")

	// beta is dropped while s1 borrows from it: the mirror stays.
	listed := map[string]Session{"s1": {LastTurn: t1}}
	res := h.run(t1, &Selection{Repositories: []Repository{h.repo("acme", "alpha", t0)}, Cleanup: cleanup(listed)})
	require.Equal(t, []string{"acme/beta"}, res.Dropped)
	require.Empty(t, res.Deleted)
	require.DirExists(t, h.mirror("acme", "beta"))
	require.Equal(t, []string{"s1"}, h.manifest().Entry("acme", "beta").BorrowedBy)

	// s1's Session is deleted: its directory goes first, then the mirror it
	// was the last to borrow from, in the same sync.
	res = h.run(t1.Add(time.Minute), &Selection{Repositories: []Repository{h.repo("acme", "alpha", t0)}, Cleanup: cleanup(nil)})
	require.Equal(t, []string{"s1"}, res.CleanedUp)
	require.Equal(t, []string{"acme/beta"}, res.Deleted)
	require.NoDirExists(t, h.mirror("acme", "beta"))
	require.Nil(t, h.manifest().Entry("acme", "beta"))
	require.NotNil(t, h.manifest().Entry("acme", "alpha"))
	entries, err := os.ReadDir(filepath.Join(h.volume, MirrorsDir, "acme"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing is left beside alpha's mirror")
}

func TestAStoppedSelectionTouchesNoMirror(t *testing.T) {
	h := newHarness(t)
	h.upstream.create("acme", "alpha")
	alpha1 := h.upstream.commit("acme", "alpha", "README.md", "alpha")
	h.run(t0, &Selection{Repositories: []Repository{h.repo("acme", "alpha", t0)}})
	dir := h.sessionDir("s1", 1)
	h.upstream.commit("acme", "alpha", "second.txt", "second")
	h.upstream.resetRequests()

	// The workspace is deleted: the sync stops, the session directories are
	// still cleaned up.
	res := h.run(t1, &Selection{Stopped: true, Cleanup: cleanup(nil)})
	require.Empty(t, res.Added)
	require.Empty(t, res.Fetched)
	require.Empty(t, res.Dropped)
	require.Zero(t, h.upstream.requestsFor("acme", "alpha"))
	require.Equal(t, alpha1, h.manifest().Entry("acme", "alpha").Head)
	require.Nil(t, h.manifest().Entry("acme", "alpha").DroppedAt)
	require.DirExists(t, dir)
	require.Len(t, h.manifest().Sessions, 1)

	_, err := h.syncer.Run(context.Background(), &Selection{Stopped: true, Repositories: []Repository{h.repo("acme", "alpha", t0)}})
	require.ErrorContains(t, err, "a stopped selection lists no repositories")
}
