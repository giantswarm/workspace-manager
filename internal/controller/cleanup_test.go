package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	var entries []mirror.Entry
	for _, it := range f.listings.items[testProvider+"/"+testOwner] {
		entries = append(entries, entry(it, 1<<20))
	}
	m := mirror.Manifest{SyncedAt: at, Repositories: entries, Sessions: dirs}
	raw, err := m.JSON()
	require.NoError(f.t, err)
	cm := f.configMap(ManifestName(ws))
	cm.Data = map[string]string{mirror.ManifestFile: string(raw)}
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
	assert.Nil(t, ws.Status.NextSync, "no schedule while a Job runs")
	require.Len(t, f.jobs(), jobs+1, "one Job for every deleted Session")
	sel := f.selection(ws)
	assert.True(t, sel.Stopped, "the Job fetches no mirror")
	assert.Empty(t, sel.Repositories)
	require.NotNil(t, sel.Cleanup)
	assert.Empty(t, sel.Cleanup.Sessions, "neither a nor b is listed: both directories go")
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
	require.NotNil(t, ws.Status.NextSync)
	assert.Equal(t, time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC), ws.Status.NextSync.UTC(), "the schedule stands")

	// Only an unknown directory is left: no look, the schedule is next.
	listed = s.count()
	f.advance(DefaultSessionPoll)
	assert.Equal(t, ws.Status.NextSync.Sub(f.clock()), f.reconcile("platform"))
	assert.Equal(t, listed, s.count())
	assert.Len(t, f.jobs(), jobs+1)
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
