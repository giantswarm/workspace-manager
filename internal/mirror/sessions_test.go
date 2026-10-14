package mirror

import (
	"context"
	"encoding/json"
	"io/fs"
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

func TestTheSyncLaysOutTheSessionsDirectory(t *testing.T) {
	h := newHarness(t)
	h.run(t2, &Selection{})

	info, err := os.Stat(filepath.Join(h.volume, SessionsDir))
	require.NoError(t, err)
	require.True(t, info.IsDir())
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
	require.Equal(t, Removed{SessionDirectories: []string{"s1"}, Mirrors: []string{"acme/beta"}}, res.Removal())
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

// otherUID makes the harness's sync run as another uid than the one owning
// what the test creates: the sync on a volume that keeps each creator's uid
// (Azure Files over NFS) beside a Session run as its own user.
func (h *harness) otherUID() Owner {
	h.t.Helper()
	if os.Getuid() == 0 {
		h.t.Skip("root reads and removes every directory")
	}
	uid := os.Getuid() + 1
	h.syncer.UID = &uid
	return Owner{UID: int64(os.Getuid()), GID: int64(os.Getgid())}
}

// runOwner is the Job's container of the directories' owner.
func (h *harness) runOwner(now time.Time, sel *Selection) *OwnerReport {
	h.t.Helper()
	owner := &Syncer{Volume: h.volume, Now: func() time.Time { return now }, Log: h.syncer.Log}
	r, err := owner.RunOwner(sel)
	require.NoError(h.t, err)
	return r
}

// TestADirectoryOfAnotherUidIsLeftToItsOwner: the sync cannot read a
// directory another uid owns; it keeps it with its last size, records the
// owner and still succeeds. The owner's container of the next Job measures
// and deletes it, and the sync takes its report into the manifest.
func TestADirectoryOfAnotherUidIsLeftToItsOwner(t *testing.T) {
	h := newHarness(t)
	owner := h.otherUID()
	dir := h.sessionDir("s1", 64)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "repo", ".git"), 0o750))
	listed := map[string]Session{"s1": {Name: "Fix it", LastTurn: t1}}

	// The Session's user alone may enter its directory.
	lock := func() { require.NoError(t, os.Chmod(dir, 0o000)) }
	unlock := func() { require.NoError(t, os.Chmod(dir, 0o700)) } //nolint:gosec // a directory's mode, which needs its x bit
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })                //nolint:gosec // a directory's mode, which needs its x bit
	lock()
	res := h.run(t0, &Selection{Cleanup: cleanup(listed)})
	require.Empty(t, res.CleanedUp)
	inv := h.inventory()
	require.Equal(t, "Fix it", inv["s1"].Name)
	require.Zero(t, inv["s1"].SizeBytes, "not measured yet")
	require.Empty(t, inv["s1"].Error, "its owner's container looks after it")
	require.Equal(t, []Owner{owner}, h.manifest().Owners)
	info, err := os.Stat(filepath.Join(h.volume, SessionsDir))
	require.NoError(t, err)
	require.Equal(t, sessionsDirMode, info.Mode()&(fs.ModePerm|fs.ModeSetgid), "the owners' group renames in sessions/")

	// The next Job: the owner's container measures it, the sync takes the
	// report.
	unlock()
	report := h.runOwner(t1, &Selection{Cleanup: cleanup(listed)})
	require.Equal(t, owner, report.Owner)
	require.Len(t, report.Sessions, 1)
	require.Equal(t, int64(64), report.Sessions[0].SizeBytes)
	h.syncer.ReportsDir = t.TempDir()
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(h.syncer.ReportsDir, ReportFile(owner)), raw, 0o600))
	lock()
	h.run(t1, &Selection{Cleanup: cleanup(listed)})
	require.Equal(t, int64(64), h.inventory()["s1"].SizeBytes)
	require.Equal(t, []Owner{owner}, h.manifest().Owners)

	// The Session is deleted: the owner's container deletes the directory,
	// and the sync lists it as cleaned up.
	unlock()
	report = h.runOwner(t2, &Selection{Cleanup: cleanup(nil)})
	require.Equal(t, []string{"s1"}, report.Deleted)
	require.NoDirExists(t, dir)
	raw, err = json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(h.syncer.ReportsDir, ReportFile(owner)), raw, 0o600))
	res = h.run(t2, &Selection{Cleanup: cleanup(nil)})
	require.Equal(t, []string{"s1"}, res.CleanedUp)
	require.Empty(t, h.manifest().Sessions)
	require.Empty(t, h.manifest().Owners, "nothing is left to another uid")
}

// TestTheOwnersContainerLooksAfterItsOwnDirectoriesAlone: a directory of
// another uid than the container's stays untouched by it.
func TestTheOwnersContainerLooksAfterItsOwnDirectoriesAlone(t *testing.T) {
	h := newHarness(t)
	h.otherUID()
	h.run(t0, &Selection{})
	dir := h.sessionDir("s1", 1)
	owner := &Syncer{Volume: h.volume, UID: h.syncer.UID, Now: func() time.Time { return t1 }, Log: h.syncer.Log}
	r, err := owner.RunOwner(&Selection{Cleanup: cleanup(nil)})
	require.NoError(t, err)
	require.Empty(t, r.Sessions)
	require.Empty(t, r.Deleted)
	require.DirExists(t, dir)
}

// TestADirectoryTheSyncCannotMeasureDoesNotFailTheSync: a directory of the
// sync's own uid it cannot measure stays with the reason; the sync goes on.
func TestADirectoryTheSyncCannotMeasureDoesNotFailTheSync(t *testing.T) {
	h := newHarness(t)
	if os.Getuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := h.sessionDir("s1", 1)
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.MkdirAll(locked, 0o700))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // a directory's mode, which needs its x bit
	h.sessionDir("s2", 8)

	h.run(t0, &Selection{Cleanup: cleanup(nil)})
	inv := h.inventory()
	require.Contains(t, inv["s1"].Error, "permission denied")
	require.Empty(t, inv["s2"].Error)
	require.Equal(t, int64(8), inv["s2"].SizeBytes)
	require.Empty(t, h.manifest().Owners, "the sync's own")
}

// TestADeletedWorkspacesUnlistedDirectoryGoesAtOnce: on a deleted
// workspace no Session starts any more, so a directory whose Session
// kagent's listing does not name goes in the first cycle, not after the
// window; a listed Session's directory keeps its window.
func TestADeletedWorkspacesUnlistedDirectoryGoesAtOnce(t *testing.T) {
	h := newHarness(t)
	stray := h.sessionDir("stray", 1)
	live := h.sessionDir("live", 1)
	c := cleanup(map[string]Session{"live": {LastTurn: t0}})
	c.Deleted = true

	res := h.run(t0.Add(time.Minute), &Selection{Stopped: true, Cleanup: c})
	require.Equal(t, []string{"stray"}, res.CleanedUp)
	require.NoDirExists(t, stray)
	require.DirExists(t, live)
	require.Equal(t, t0.Add(window), h.inventory()["live"].CleanupAfter)
	require.Contains(t, h.logs.String(), "the workspace is deleted, and kagent does not list the Session")
}
