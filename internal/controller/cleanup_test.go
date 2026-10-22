package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/kagent"
	"github.com/giantswarm/workspace-manager/internal/mirror"
)

const (
	testDriver = "efs.csi.aws.com"
	testHandle = "fs-0123::fsap-platform"
)

// sessions is a Sessions over a fixed listing, counting its calls.
type sessions struct {
	mu    sync.Mutex
	list  map[string]kagent.Session
	calls int
	err   error
}

func (s *sessions) Sessions(context.Context) ([]kagent.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	out := make([]kagent.Session, 0, len(s.list))
	for _, sess := range s.list {
		out = append(out, sess)
	}
	return out, nil
}

func (s *sessions) put(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list[id] = kagent.Session{ID: id, Name: "Session " + id, Creator: "you@example.com", CreatedAt: testNow.Add(-time.Hour),
		LastTurn: testNow.Add(-time.Minute), Driver: testDriver, Handle: testHandle}
}

func (s *sessions) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.list, id)
}

func (s *sessions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// withSessions gives the fixture's controller a kagent listing and runs the
// Workspace's first cycle to a manifest with the directories of Sessions a
// and b, which kagent lists, and of u, which it does not know; the claim is
// bound to its CSI volume, which the Sessions name.
func (f *fixture) withSessions(name string) (*sessions, *v1alpha1.Workspace) {
	f.t.Helper()
	s := &sessions{list: map[string]kagent.Session{}}
	s.put("a")
	s.put("b")
	f.c.sessions = s

	ws := f.firstCycle(name)
	ctx := context.Background()
	_, err := f.core.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:               corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: testDriver, VolumeHandle: testHandle}},
		},
	}, metav1.CreateOptions{})
	require.NoError(f.t, err)
	claim, err := f.core.CoreV1().PersistentVolumeClaims(testNamespace).Get(ctx, ClaimName(ws), metav1.GetOptions{})
	require.NoError(f.t, err)
	claim.Spec.VolumeName = "pv-" + name
	_, err = f.core.CoreV1().PersistentVolumeClaims(testNamespace).Update(ctx, claim, metav1.UpdateOptions{})
	require.NoError(f.t, err)

	f.finishCycle(ws, testNow, directory("a", true), directory("b", true), directory("u", false))
	ws = f.workspace(name)
	require.Len(f.t, ws.Status.Sessions, 3)
	return s, ws
}

// directory is a session directory the manifest records.
func directory(id string, known bool) mirror.SessionDirectory {
	return mirror.SessionDirectory{Session: id, Known: known, FirstSeen: testNow, SizeBytes: 1 << 10}
}

// finishCycle ends the active Job with a manifest of the session
// directories and the listed repositories, and reconciles the outcome.
func (f *fixture) finishCycle(ws *v1alpha1.Workspace, at time.Time, dirs ...mirror.SessionDirectory) {
	f.t.Helper()
	f.finishCycleRemoving(ws, at, mirror.Removed{}, dirs...)
}

// finishCycleRemoving is finishCycle with what the Job removed written beside
// its manifest, as the sync writes it.
func (f *fixture) finishCycleRemoving(ws *v1alpha1.Workspace, at time.Time, removed mirror.Removed, dirs ...mirror.SessionDirectory) {
	f.t.Helper()
	var entries []mirror.Entry
	for _, it := range f.listings.items[testProvider+"/"+testOwner] {
		entries = append(entries, entry(it, 1<<20))
	}
	m := mirror.Manifest{SyncedAt: at, Repositories: entries, Sessions: dirs}
	raw, err := m.JSON()
	require.NoError(f.t, err)
	rawRemoved, err := json.Marshal(removed)
	require.NoError(f.t, err)
	cm := f.configMap(ManifestName(ws))
	cm.Data = map[string]string{mirror.ManifestFile: string(raw), mirror.RemovedKey: string(rawRemoved)}
	_, err = f.core.CoreV1().ConfigMaps(testNamespace).Update(context.Background(), cm, metav1.UpdateOptions{})
	require.NoError(f.t, err)
	f.finishJob(ws, batchv1.JobComplete, at)
	f.reconcile(ws.Name)
}

// TestADeletedSessionsDirectoryGoesAtOnce: with no cycle due, a Session
// kagent no longer lists has its directory deleted within the poll, by one
// cleanup-only Job for all the workspace's deleted Sessions that lists no
// provider and fetches no mirror; one kagent listing per poll serves every
// look, and a directory kagent never knew waits for its window.
func TestADeletedSessionsDirectoryGoesAtOnce(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	s, ws := f.withSessions("platform")
	require.Nil(t, ws.Status.ActiveSync)
	listed := s.count()
	providerListings := f.listings.count()
	jobs := len(f.jobs())

	// Every Session is listed: the next look is a poll away, and a look
	// within the poll reads no listing.
	assert.Equal(t, DefaultSessionPoll, f.reconcile("platform"))
	assert.Equal(t, listed, s.count(), "the listing read at the cycle's end serves the poll")
	assert.Len(t, f.jobs(), jobs)

	// a and b are deleted in kagent: at the next poll one Job deletes both.
	s.delete("a")
	s.delete("b")
	f.advance(DefaultSessionPoll)
	assert.Zero(t, f.reconcile("platform"), "a running Job wakes the controller by itself")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	assert.Equal(t, v1alpha1.TriggerSessionDeleted, ws.Status.ActiveSync.Trigger)
	assert.True(t, ws.Status.ActiveSync.CleanupOnly)
	require.Len(t, f.jobs(), jobs+1, "one Job for every deleted Session")
	sel := f.selection(ws)
	assert.True(t, sel.Stopped, "the Job fetches no mirror")
	assert.Empty(t, sel.Repositories)
	require.NotNil(t, sel.Cleanup)
	assert.Empty(t, sel.Cleanup.Sessions, "neither a nor b is listed: both directories go")
	assert.False(t, sel.Cleanup.Deleted, "a live workspace's unknown directory keeps its window")
	assert.Equal(t, providerListings, f.listings.count(), "no provider is listed")

	// While the Job runs, a look starts no second one.
	f.advance(DefaultSessionPoll)
	assert.Zero(t, f.reconcile("platform"))
	assert.Len(t, f.jobs(), jobs+1)

	// The Job leaves u, which waits for its window.
	ended := f.clock()
	f.finishCycle(ws, ended, directory("u", false))
	ws = f.workspace("platform")
	assert.Nil(t, ws.Status.ActiveSync)
	require.NotNil(t, ws.Status.LastSync)
	assert.Equal(t, v1alpha1.TriggerSessionDeleted, ws.Status.LastSync.Trigger)
	assert.Equal(t, v1alpha1.SyncNoChange, ws.Status.LastSync.Result)
	require.Len(t, ws.Status.Sessions, 1)
	assert.Equal(t, "u", ws.Status.Sessions[0].Session)
	cond := syncedCondition(t, ws)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Contains(t, cond.Message, "Sessions deleted in kagent")

	// Only an unknown directory without a cleanup date is left: no look,
	// nothing due.
	listed = s.count()
	f.advance(DefaultSessionPoll)
	assert.Zero(t, f.reconcile("platform"))
	assert.Equal(t, listed, s.count())
	assert.Len(t, f.jobs(), jobs+1)
}

// TestACleanupOnlyCycleRecordsWhatItRemoved: a cycle whose selection equals
// the last manifest only looks after the session directories and the
// dropped mirrors. It records NoChange when its Job removed nothing, and
// Succeeded with the counts, in the last sync and the Synced condition, when
// the Job deleted a session directory or dropped a mirror.
func TestACleanupOnlyCycleRecordsWhatItRemoved(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	f.withSessions("platform")

	f.requestSync("platform")
	f.reconcile("platform")
	ws := f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	require.True(t, ws.Status.ActiveSync.CleanupOnly, "nothing changed: the Job only looks after the volume")
	job := ws.Status.ActiveSync.Job
	f.finishCycleRemoving(ws, f.clock(), mirror.Removed{SessionDirectories: []string{"a", "b", "old"}, Mirrors: []string{"acme/legacy"}}, directory("u", false))
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.LastSync)
	assert.Equal(t, v1alpha1.SyncSucceeded, ws.Status.LastSync.Result, "the Job removed data: a change")
	assert.Equal(t, "removed 3 session directories, dropped 1 mirror", ws.Status.LastSync.Message)
	cond := syncedCondition(t, ws)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, v1alpha1.ReasonSynced, cond.Reason)
	assert.Equal(t, "the selection equals the last manifest and nothing was pushed since; sync job "+job+
		" looked after the session directories; removed 3 session directories, dropped 1 mirror", cond.Message)

	f.advance(time.Minute)
	f.requestSync("platform")
	f.reconcile("platform")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	require.True(t, ws.Status.ActiveSync.CleanupOnly)
	f.finishCycle(ws, f.clock(), directory("u", false))
	ws = f.workspace("platform")
	assert.Equal(t, v1alpha1.SyncNoChange, ws.Status.LastSync.Result, "the Job removed nothing")
	assert.Empty(t, ws.Status.LastSync.Message)
	cond = syncedCondition(t, ws)
	assert.Equal(t, v1alpha1.ReasonNoChange, cond.Reason)
	assert.NotContains(t, cond.Message, "removed")
}

// TestADirectoryGoesAtItsCleanupDate: with no sync due, a live workspace's
// session directory goes at its own cleanup date, in a cleanup-only Job that
// lists no provider and fetches no mirror; a directory the Job leaves behind
// is not looked after again before the resync.
func TestADirectoryGoesAtItsCleanupDate(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	f.withSessions("platform")
	f.requestSync("platform")
	f.reconcile("platform")
	ws := f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	due := f.clock().Add(2 * time.Hour)
	u := directory("u", false)
	u.CleanupAfter = due
	f.finishCycle(ws, f.clock(), u)
	ws = f.workspace("platform")
	require.Len(t, ws.Status.Sessions, 1)
	require.NotNil(t, ws.Status.Sessions[0].CleanupAfter)
	listings, jobs := f.listings.count(), len(f.jobs())

	assert.Equal(t, 2*time.Hour, f.reconcile("platform"), "the controller wakes at the cleanup date")
	assert.Len(t, f.jobs(), jobs)

	f.advance(2 * time.Hour)
	assert.Zero(t, f.reconcile("platform"), "a running Job wakes the controller by itself")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	assert.Equal(t, v1alpha1.TriggerCleanup, ws.Status.ActiveSync.Trigger)
	assert.True(t, ws.Status.ActiveSync.CleanupOnly)
	require.Len(t, f.jobs(), jobs+1)
	sel := f.selection(ws)
	assert.True(t, sel.Stopped, "the Job fetches no mirror")
	assert.Empty(t, sel.Repositories)
	require.NotNil(t, sel.Cleanup, "kagent's Sessions are listed for the cleanup")
	assert.Equal(t, listings, f.listings.count(), "no provider is listed")

	// The Job leaves u behind: no second Job before the resync.
	f.finishCycle(ws, f.clock(), u)
	ws = f.workspace("platform")
	assert.Equal(t, v1alpha1.TriggerCleanup, ws.Status.LastSync.Trigger)
	assert.Equal(t, v1alpha1.SyncNoChange, ws.Status.LastSync.Result)
	assert.Contains(t, syncedCondition(t, ws).Message, "past their cleanup date")
	assert.Equal(t, time.Hour, f.reconcile("platform"))
	assert.Len(t, f.jobs(), jobs+1)

	f.advance(time.Hour)
	f.reconcile("platform")
	require.Len(t, f.jobs(), jobs+2, "again at the resync")
	assert.Equal(t, v1alpha1.TriggerCleanup, f.workspace("platform").Status.ActiveSync.Trigger)
}

// TestADeletedSessionWaitsWhileKagentFails: a failed listing deletes
// nothing and looks again at the next poll.
func TestADeletedSessionWaitsWhileKagentFails(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	s, _ := f.withSessions("platform")
	jobs := len(f.jobs())
	s.delete("a")
	s.mu.Lock()
	s.err = errors.New("kagent unavailable")
	s.mu.Unlock()

	f.advance(DefaultSessionPoll)
	assert.Equal(t, DefaultSessionPoll, f.reconcile("platform"))
	assert.Len(t, f.jobs(), jobs)
	assert.Nil(t, f.workspace("platform").Status.ActiveSync)
}

// TestARefusedStatusStartsNoJob: when the API server refuses the status
// naming the cleanup's Job (a CRD older than the manager), no Job starts, so
// the retries leave none behind.
func TestARefusedStatusStartsNoJob(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	s, _ := f.withSessions("platform")
	jobs := len(f.jobs())
	f.dyn.PrependReactor("update", "workspaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInvalid(v1alpha1.GroupVersion.WithKind("Workspace").GroupKind(), "platform",
			field.ErrorList{field.NotSupported(field.NewPath("status", "activeSync", "trigger"), "sessionDeleted", []string{"schedule"})})
	})
	s.delete("a")
	f.advance(DefaultSessionPoll)
	for range 3 {
		_, err := f.c.Reconcile(context.Background(), f.workspace("platform"))
		require.Error(t, err)
	}
	assert.Len(t, f.jobs(), jobs)
}

// TestADeletedSessionWaitsForTheRetry: after a failed cycle the retry, whose
// Job cleans up too, comes first; no cleanup-only Job runs before it.
func TestADeletedSessionWaitsForTheRetry(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	s, _ := f.withSessions("platform")
	f.listings.mu.Lock()
	f.listings.err = errors.New("provider unavailable")
	f.listings.mu.Unlock()
	f.updateSpec("platform", func(w *v1alpha1.Workspace) { w.Spec.Sources[0].Repositories = []string{"api"} })
	f.reconcile("platform")
	require.Equal(t, v1alpha1.SyncFailed, f.workspace("platform").Status.LastSync.Result)
	jobs := len(f.jobs())
	listed := s.count()

	s.delete("a")
	f.advance(DefaultSessionPoll)
	f.reconcile("platform")
	assert.Len(t, f.jobs(), jobs)
	assert.Equal(t, listed, s.count(), "no listing before the retry")
}

// TestARequestRunsADeletingWorkspacesCycleNow: a deleting workspace whose
// last deletion cycle kept a directory waits for the recheck;
// a request (sync_workspace, or delete_workspace again) runs the cycle at
// once. A directory still there is kept as by any cycle; once one finds
// none left, the claim and the finalizer go.
func TestARequestRunsADeletingWorkspacesCycleNow(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	f.withSessions("platform")
	ws := f.workspace("platform")
	ws.DeletionTimestamp = &metav1.Time{Time: f.clock()}
	_, err := f.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Update(context.Background(), toUnstructured(t, ws), metav1.UpdateOptions{})
	require.NoError(t, err)

	// The first deletion cycle keeps u (the Job could not delete it).
	f.reconcile("platform")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	require.Equal(t, v1alpha1.TriggerDeletion, ws.Status.ActiveSync.Trigger)
	f.finishCycle(ws, f.clock(), directory("u", false))
	jobs := len(f.jobs())
	wait := f.reconcile("platform")
	assert.Equal(t, DeletionRecheck, wait, "the next deletion cycle is the recheck's")
	assert.Len(t, f.jobs(), jobs)

	request := func() { f.requestSync("platform") }

	// A request while u is still there: the cycle runs now, with the
	// cleanup's window for a listed Session's directory; u kept again, the
	// workspace and its claim stay.
	f.advance(time.Minute)
	request()
	assert.Zero(t, f.reconcile("platform"), "a running Job wakes the controller by itself")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync, "the request runs the deletion cycle now")
	assert.Equal(t, v1alpha1.TriggerDeletion, ws.Status.ActiveSync.Trigger)
	assert.True(t, ws.Status.ActiveSync.CleanupOnly)
	assert.Empty(t, ws.Annotations[v1alpha1.AnnotationSyncRequested], "the request is taken")
	assert.Empty(t, ws.Annotations[v1alpha1.AnnotationSyncRequestedBy])
	require.Len(t, f.jobs(), jobs+1)
	sel := f.selection(ws)
	assert.True(t, sel.Stopped)
	assert.Empty(t, sel.Repositories)
	require.NotNil(t, sel.Cleanup)
	assert.Equal(t, int64(DefaultSessionCleanupAfter/time.Second), sel.Cleanup.AfterSeconds, "the window holds")
	assert.True(t, sel.Cleanup.Deleted, "a directory whose Session kagent does not list goes at once")
	f.finishCycle(ws, f.clock(), directory("u", false))
	assert.Positive(t, f.reconcile("platform"))
	ws = f.workspace("platform")
	assert.Contains(t, ws.Finalizers, v1alpha1.FinalizerVolume)
	assert.Len(t, f.claims(), 1, "the volume stays while a directory is left")

	// u is cleared outside a cycle; a request finds it gone at once, and the
	// claim and the finalizer go.
	f.advance(time.Minute)
	request()
	f.reconcile("platform")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	require.Len(t, f.jobs(), jobs+2)
	f.finishCycle(ws, f.clock())
	assert.Empty(t, f.claims(), "the claim is deleted")
	assert.NotContains(t, f.workspace("platform").Finalizers, v1alpha1.FinalizerVolume, "the finalizer is removed")
}

// TestADeletedSessionRunsADeletingWorkspacesCycle: a deleting workspace whose
// last deletion cycle kept the directory of a Session kagent lists looks for
// its deletion within the poll; once kagent no longer lists it, the deletion
// cycle runs at once, and the claim and the finalizer go after it. A failed
// listing waits for the next poll.
func TestADeletedSessionRunsADeletingWorkspacesCycle(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	s, ws := f.withSessions("platform")
	ws.DeletionTimestamp = &metav1.Time{Time: f.clock()}
	_, err := f.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Update(context.Background(), toUnstructured(t, ws), metav1.UpdateOptions{})
	require.NoError(t, err)

	// The first deletion cycle keeps a, whose Session kagent lists.
	f.reconcile("platform")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	f.finishCycle(ws, f.clock(), directory("a", true))
	jobs := len(f.jobs())
	assert.Equal(t, DefaultSessionPoll, f.reconcile("platform"), "a kept Session's deletion is looked for within the poll")
	assert.Len(t, f.jobs(), jobs)

	// A failed listing starts nothing and looks again at the next poll.
	s.delete("a")
	s.mu.Lock()
	s.err = errors.New("kagent unavailable")
	s.mu.Unlock()
	f.advance(DefaultSessionPoll)
	assert.Equal(t, DefaultSessionPoll, f.reconcile("platform"))
	assert.Len(t, f.jobs(), jobs)

	// a is deleted in kagent: the deletion cycle runs now, not at the recheck.
	s.mu.Lock()
	s.err = nil
	s.mu.Unlock()
	f.advance(DefaultSessionPoll)
	assert.Zero(t, f.reconcile("platform"), "a running Job wakes the controller by itself")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync, "the deleted Session runs the deletion cycle")
	assert.Equal(t, v1alpha1.TriggerDeletion, ws.Status.ActiveSync.Trigger)
	require.Len(t, f.jobs(), jobs+1)
	sel := f.selection(ws)
	require.NotNil(t, sel.Cleanup)
	assert.True(t, sel.Cleanup.Deleted)
	assert.NotContains(t, sel.Cleanup.Sessions, "a", "a's directory goes")

	f.finishCycle(ws, f.clock())
	assert.Empty(t, f.claims(), "the claim is deleted")
	assert.NotContains(t, f.workspace("platform").Finalizers, v1alpha1.FinalizerVolume, "the finalizer is removed")
}

// TestADeletingWorkspaceStartsOneDeletionJob: where the status names no Job
// (a CRD that prunes status.activeSync), a deleting workspace's every event
// would start one more deletion Job, since no cycle's end is ever recorded;
// one runs, and the next waits for its end.
func TestADeletingWorkspaceStartsOneDeletionJob(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	f.withSessions("platform")
	jobs := len(f.jobs())
	f.pruneActiveSync()
	ws := f.workspace("platform")
	ws.DeletionTimestamp = &metav1.Time{Time: f.clock()}
	_, err := f.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Update(context.Background(), toUnstructured(t, ws), metav1.UpdateOptions{})
	require.NoError(t, err)

	for range 3 {
		f.reconcile("platform")
	}
	assert.Equal(t, jobs+1, len(f.jobs()), "one deletion Job")
	assert.Equal(t, 1, len(f.running()))
}

// TestADirectoryLeftWithAnErrorIsReportedAndWaits: a directory the cycle
// could neither measure nor delete is named on SessionsCleanedUp, and a
// deleted workspace's next cleanup waits for the recheck instead of
// retrying it at once.
func TestADirectoryLeftWithAnErrorIsReportedAndWaits(t *testing.T) {
	ws := workspaceNamed("platform")
	due := metav1.NewTime(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	sessions := sessionDirectories(&mirror.Manifest{Sessions: []mirror.SessionDirectory{
		{Session: "ok", SizeBytes: 1},
		{Session: "stuck", CleanupAfter: due.Time, Error: "measure /workspace/sessions/stuck: permission denied"},
	}})
	assert.Equal(t, "measure /workspace/sessions/stuck: permission denied", sessions[1].Error)
	cond := directoriesLeft(ws, sessions)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v1alpha1.ReasonDirectoriesLeft, cond.Reason)
	assert.Contains(t, cond.Message, "stuck: measure /workspace/sessions/stuck: permission denied")
	assert.Nil(t, directoriesLeft(ws, sessions[:1]))

	c := &Controller{resync: time.Hour}
	ws.Status.Sessions = sessions
	last := due.Add(time.Minute)
	assert.Equal(t, last.Add(DeletionRecheck), c.nextDeletionCycle(ws, last), "the stuck directory's past date does not wake the cycle")
	sessions[1].Error = ""
	assert.Equal(t, due.Time, c.nextDeletionCycle(ws, last))
}
