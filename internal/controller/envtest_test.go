//go:build envtest

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/kagent"
	"github.com/giantswarm/workspace-manager/internal/kagent/kagenttest"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

// cleanupLab is the controller against a real API server, a fake kagent and
// a volume in a directory: the sync Job is played by running the sync on
// that directory with the Job's own input, writing its result where the Job
// writes it.
type cleanupLab struct {
	t      *testing.T
	ctx    context.Context
	c      *Controller
	core   kubernetes.Interface
	dyn    dynamic.Interface
	kagent *kagenttest.Server
	volume string
	logs   *bytes.Buffer
	mu     sync.Mutex
	now    time.Time
}

const (
	labWorkspace = "otter-mcp"
	labDriver    = "efs.csi.aws.com"
	labHandle    = "fs-0123::fsap-otter"
	labWindow    = time.Hour
)

func newCleanupLab(t *testing.T) *cleanupLab {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "helm", "workspace-manager", "files", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "envtest needs KUBEBUILDER_ASSETS (make test-envtest sets it)")
	t.Cleanup(func() { _ = env.Stop() })
	ctx := context.Background()
	core, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	_, err = core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = core.StorageV1().StorageClasses().Create(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: testClass}, Provisioner: labDriver}, metav1.CreateOptions{})
	require.NoError(t, err)

	l := &cleanupLab{t: t, ctx: ctx, core: core, dyn: dyn, kagent: kagenttest.New(t), volume: t.TempDir(), logs: &bytes.Buffer{}, now: testNow}
	sessions, err := kagent.Dial(l.kagent.Target, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sessions.Close() })
	l.c = New(Config{
		Dynamic: dyn, Core: core, Namespace: testNamespace, StorageClass: testClass, Resync: 10 * time.Minute,
		SessionCleanupAfter: labWindow,
		Sessions:            sessions,
		Listings:            &listings{items: map[string][]provider.Item{}},
		Instances:           []provider.Instance{{Name: testProvider, KindName: "stub", Kind: stubKind{}}},
		SyncServiceAccount:  "workspace-manager-sync", SyncImage: "gsoci.azurecr.io/giantswarm/workspace-manager:test",
		Now:    l.clock,
		Logger: slog.New(slog.NewTextHandler(l.logs, nil)),
	})
	return l
}

func (l *cleanupLab) clock() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.now
}

func (l *cleanupLab) advance(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = l.now.Add(d)
}

func (l *cleanupLab) workspace() *v1alpha1.Workspace {
	l.t.Helper()
	u, err := l.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Get(l.ctx, labWorkspace, metav1.GetOptions{})
	require.NoError(l.t, err)
	ws, err := fromUnstructured(u)
	require.NoError(l.t, err)
	return ws
}

func (l *cleanupLab) reconcile() time.Duration {
	l.t.Helper()
	requeue, err := l.c.Reconcile(l.ctx, l.workspace())
	require.NoError(l.t, err)
	return requeue
}

// runJob plays the active sync Job: the sync on the volume with the Job's
// selection, its manifest written into the result ConfigMap, the Job gone
// after; then the controller records the outcome.
func (l *cleanupLab) runJob() *mirror.Result {
	l.t.Helper()
	ws := l.workspace()
	require.NotNil(l.t, ws.Status.ActiveSync, "a sync Job runs")
	_, err := l.core.BatchV1().Jobs(testNamespace).Get(l.ctx, ws.Status.ActiveSync.Job, metav1.GetOptions{})
	require.NoError(l.t, err)
	cm, err := l.core.CoreV1().ConfigMaps(testNamespace).Get(l.ctx, SelectionName(ws), metav1.GetOptions{})
	require.NoError(l.t, err)
	var sel mirror.Selection
	require.NoError(l.t, json.Unmarshal([]byte(cm.Data[selectionFile]), &sel))

	l.advance(time.Second)
	syncer := &mirror.Syncer{Volume: l.volume, Now: l.clock, Log: slog.New(slog.NewTextHandler(l.logs, nil))}
	res, err := syncer.Run(l.ctx, &sel)
	require.NoError(l.t, err)
	raw, err := res.Manifest.JSON()
	require.NoError(l.t, err)
	result, err := l.core.CoreV1().ConfigMaps(testNamespace).Get(l.ctx, ManifestName(ws), metav1.GetOptions{})
	require.NoError(l.t, err)
	result.Data = map[string]string{mirror.ManifestFile: string(raw)}
	_, err = l.core.CoreV1().ConfigMaps(testNamespace).Update(l.ctx, result, metav1.UpdateOptions{})
	require.NoError(l.t, err)
	background := metav1.DeletePropagationBackground
	require.NoError(l.t, l.core.BatchV1().Jobs(testNamespace).Delete(l.ctx, ws.Status.ActiveSync.Job, metav1.DeleteOptions{PropagationPolicy: &background}))
	l.advance(time.Second)
	l.reconcile()
	return res
}

// requestSync is sync_workspace: the request annotation.
func (l *cleanupLab) requestSync() {
	l.t.Helper()
	patch := `{"metadata":{"annotations":{"` + v1alpha1.AnnotationSyncRequested + `":"` + l.clock().Format(time.RFC3339) + `"}}}`
	_, err := l.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Patch(l.ctx, labWorkspace, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	require.NoError(l.t, err)
}

// sessionDir makes a session directory with a file of size bytes.
func (l *cleanupLab) sessionDir(id string, size int) string {
	l.t.Helper()
	dir := filepath.Join(l.volume, mirror.SessionsDir, id)
	require.NoError(l.t, os.MkdirAll(dir, 0o700))
	require.NoError(l.t, os.WriteFile(filepath.Join(dir, "work.txt"), make([]byte, size), 0o600))
	return dir
}

func (l *cleanupLab) inventory() map[string]v1alpha1.SessionDirectory {
	out := map[string]v1alpha1.SessionDirectory{}
	for _, d := range l.workspace().Status.Sessions {
		out[d.Session] = d
	}
	return out
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// TestSessionDirectoriesAreCleanedUp walks the cleanup through a workspace's
// life on a real API server with a fake kagent and a one-hour window: an old
// directory goes and a newer one stays, a Session deleted in kagent loses its
// directory at the next run, a dropped mirror stays while borrowed and goes
// after, the status lists the directories, and a deleted workspace's volume
// goes with its last directory, not before.
func TestSessionDirectoriesAreCleanedUp(t *testing.T) {
	l := newCleanupLab(t)
	now := l.clock()

	// The volume as a previous sync left it: acme/old mirrored, three
	// session directories, one of them borrowing from the mirror.
	mirrorDir := filepath.Join(l.volume, mirror.MirrorsDir, testOwner, "old.git")
	require.NoError(t, os.MkdirAll(mirrorDir, 0o755))
	gitCmd(t, mirrorDir, "init", "--quiet", "--bare")
	require.NoError(t, (&mirror.Manifest{SyncedAt: now.Add(-24 * time.Hour), Repositories: []mirror.Entry{{Provider: testProvider, Owner: testOwner, Name: "old",
		Path: "mirrors/acme/old.git", CloneURL: "https://code.test/acme/old.git", DefaultBranch: "main", PushedAt: now.Add(-48 * time.Hour), SizeBytes: 100}}}).Write(l.volume))
	oldDir := l.sessionDir("11111111-0000-0000-0000-000000000001", 10)
	newerDir := l.sessionDir("11111111-0000-0000-0000-000000000002", 4096)
	doomedDir := l.sessionDir("11111111-0000-0000-0000-000000000003", 20)
	gitCmd(t, doomedDir, "clone", "--quiet", "--shared", "--no-checkout", mirrorDir, "old")
	onVolume := func(id, name string, lastTurn time.Time) kagent.Session {
		return kagent.Session{ID: id, Name: name, Creator: "you@example.com", CreatedAt: now.Add(-3 * labWindow), LastTurn: lastTurn, Driver: labDriver, Handle: labHandle}
	}
	l.kagent.Put(onVolume("11111111-0000-0000-0000-000000000001", "Review PR", now.Add(-2*labWindow)))
	l.kagent.Put(onVolume("11111111-0000-0000-0000-000000000002", "Fix the flaky test", now.Add(-10*time.Minute)))
	l.kagent.Put(onVolume("11111111-0000-0000-0000-000000000003", "Bump deps", now.Add(-5*time.Minute)))
	// A Session on another workspace's volume is not this workspace's.
	l.kagent.Put(kagent.Session{ID: "22222222-0000-0000-0000-000000000001", LastTurn: now, Driver: labDriver, Handle: "fs-0123::fsap-other"})

	ws := workspaceNamed(labWorkspace)
	ws.UID, ws.Generation, ws.ResourceVersion = "", 0, ""
	_, err := l.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Create(l.ctx, toUnstructured(t, ws), metav1.CreateOptions{})
	require.NoError(t, err)

	// First cycle: the finalizer goes on, the volume is claimed; its CSI
	// handle is not known yet, so nothing is deleted, every directory is
	// measured, and the dropped mirror stays for its borrower.
	l.reconcile()
	ws = l.workspace()
	require.Contains(t, ws.Finalizers, v1alpha1.FinalizerVolume)
	cond := meta.FindStatusCondition(ws.Status.Conditions, v1alpha1.ConditionSessionsCleanedUp)
	require.Equal(t, v1alpha1.ReasonVolumePending, cond.Reason)
	res := l.runJob()
	require.Empty(t, res.CleanedUp)
	require.Equal(t, []string{"acme/old"}, res.Dropped)
	require.Len(t, l.inventory(), 3, "the status lists every directory before the cleanup")
	require.DirExists(t, oldDir)

	// The claim binds to its CSI volume, which the Sessions name.
	claim, err := l.core.CoreV1().PersistentVolumeClaims(testNamespace).Get(l.ctx, ClaimName(ws), metav1.GetOptions{})
	require.NoError(t, err)
	_, err = l.core.CoreV1().PersistentVolumes().Create(l.ctx, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-otter"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:               corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: labDriver, VolumeHandle: labHandle}},
			StorageClassName:       testClass,
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	claim.Spec.VolumeName = "pv-otter"
	_, err = l.core.CoreV1().PersistentVolumeClaims(testNamespace).Update(l.ctx, claim, metav1.UpdateOptions{})
	require.NoError(t, err)

	// An old directory goes, a newer one stays: nothing changed in the
	// selection, so the Job only looks after the session directories.
	l.requestSync()
	l.reconcile()
	ws = l.workspace()
	require.True(t, ws.Status.ActiveSync.CleanupOnly)
	cond = meta.FindStatusCondition(ws.Status.Conditions, v1alpha1.ConditionSessionsCleanedUp)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Contains(t, cond.Message, "kagent lists 3 Sessions on the volume")
	res = l.runJob()
	require.Equal(t, []string{"11111111-0000-0000-0000-000000000001"}, res.CleanedUp)
	require.NoDirExists(t, oldDir)
	require.DirExists(t, newerDir)
	require.DirExists(t, mirrorDir, "the dropped mirror stays while borrowed")
	ws = l.workspace()
	require.Equal(t, v1alpha1.SyncNoChange, ws.Status.LastSync.Result)
	inv := l.inventory()
	require.Len(t, inv, 2)
	require.NotContains(t, inv, "11111111-0000-0000-0000-000000000001", "the status no longer lists the deleted directory")
	newer := inv["11111111-0000-0000-0000-000000000002"]
	require.Equal(t, "Fix the flaky test", newer.Name)
	require.Equal(t, "you@example.com", newer.Creator)
	require.False(t, newer.Unknown)
	require.True(t, newer.LastTurn.Time.Equal(now.Add(-10*time.Minute)))
	require.True(t, newer.CleanupAfter.Time.Equal(now.Add(-10*time.Minute+labWindow)), "cleanup %s", newer.CleanupAfter)
	require.Equal(t, int64(4096), newer.Size.Value())

	// The Session borrowing from the dropped mirror is deleted in kagent: its
	// directory goes at the next run, and the mirror with it.
	l.kagent.Delete("11111111-0000-0000-0000-000000000003")
	l.requestSync()
	l.reconcile()
	res = l.runJob()
	require.Equal(t, []string{"11111111-0000-0000-0000-000000000003"}, res.CleanedUp)
	require.Equal(t, []string{"acme/old"}, res.Deleted)
	require.NoDirExists(t, doomedDir)
	require.NoDirExists(t, mirrorDir)
	require.Len(t, l.inventory(), 1)

	// The workspace is deleted: it stays, and its volume with it, while a
	// session directory is left.
	require.NoError(t, l.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Delete(l.ctx, labWorkspace, metav1.DeleteOptions{}))
	l.reconcile()
	ws = l.workspace()
	require.Equal(t, v1alpha1.TriggerDeletion, ws.Status.ActiveSync.Trigger)
	l.runJob()
	ws = l.workspace()
	require.Len(t, ws.Status.Sessions, 1)
	require.Equal(t, v1alpha1.ReasonDeleting, meta.FindStatusCondition(ws.Status.Conditions, v1alpha1.ConditionSynced).Reason)
	requeue := l.reconcile()
	require.Positive(t, requeue)
	require.LessOrEqual(t, requeue, newer.CleanupAfter.Sub(l.clock()), "the next run is due at the directory's cleanup date")
	claim, err = l.core.CoreV1().PersistentVolumeClaims(testNamespace).Get(l.ctx, ClaimName(ws), metav1.GetOptions{})
	require.NoError(t, err)
	require.Nil(t, claim.DeletionTimestamp, "the volume stays while a session directory is left")
	require.DirExists(t, newerDir)

	// The last directory's window passes: it goes, and the volume and the
	// workspace with it.
	l.advance(requeue)
	l.reconcile()
	l.runJob()
	require.NoDirExists(t, newerDir)
	claim, err = l.core.CoreV1().PersistentVolumeClaims(testNamespace).Get(l.ctx, ClaimName(ws), metav1.GetOptions{})
	if err == nil {
		require.NotNil(t, claim.DeletionTimestamp, "the claim is deleted (the API server's pvc-protection finalizer holds it)")
	} else {
		require.True(t, apierrors.IsNotFound(err))
	}
	_, err = l.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Get(l.ctx, labWorkspace, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "the workspace is gone with its finalizer: %v", err)
	require.Contains(t, l.logs.String(), "workspace volume deleted: its last session directory is gone")
}
