//go:build envtest

package kinds

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/controller"
	"github.com/giantswarm/workspace-manager/internal/kube"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

// syncClass provisions a size and expands online (Azure Files): the
// controller sizes and grows the claim on it.
const syncClass = "azurefile-nfs"

// syncNow is a Wednesday at noon UTC: the nightly sync is due at 03:00 the
// next day.
var syncNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// stoppedClock is the controller's clock in the test: it moves when advanced.
type stoppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stoppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stoppedClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// TestSyncCycleEnvtest runs the controller against a real API server serving
// the chart's CRD, with the GitHub stub as the provider: a workspace syncs
// after its creation and on a sync request, never on a schedule; the no-op
// check costs no Job when nothing changed; a push makes the next cycle's Job
// fetch that repository alone; a repository gaining the topic joins at the
// next cycle; and the volume is claimed and grown per the sizing rule on a
// class that provisions a size.
func TestSyncCycleEnvtest(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "helm", "workspace-manager", "files", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "envtest needs KUBEBUILDER_ASSETS (make test-envtest sets it)")
	t.Cleanup(func() { _ = env.Stop() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	core, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	_, err = core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wsNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = core.StorageV1().StorageClasses().Create(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: syncClass},
		Provisioner: "file.csi.azure.com", AllowVolumeExpansion: ptr.To(true)}, metav1.CreateOptions{})
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	f := newToolsFixture(t, kubeClients{dyn: dyn, core: core})

	clock := &stoppedClock{now: syncNow}
	ctl := controller.New(controller.Config{
		Dynamic: dyn, Core: core, Namespace: wsNamespace, StorageClass: syncClass,
		// The resync is the test's heartbeat: the clock moves, the next
		// resync reconciles. So short, it also queues a Workspace again
		// while its cycle writes the status, which a controller reading
		// the informer's cache would serve stale and start twice.
		Resync:             2 * time.Second,
		Listings:           workspace.NewListings(f.cfg.Providers, f.cfg.Secrets),
		Instances:          f.cfg.Providers,
		Secrets:            f.cfg.Secrets,
		SyncServiceAccount: "workspace-manager-sync",
		SyncImage:          "gsoci.azurecr.io/giantswarm/workspace-manager:test",
		Now:                clock.Now,
		Logger:             slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	done := make(chan error, 1)
	go func() { done <- ctl.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		assert.NoError(t, <-done)
	})

	get := func() *v1alpha1.Workspace {
		t.Helper()
		u, err := dyn.Resource(v1alpha1.WorkspaceResource).Namespace(wsNamespace).Get(ctx, "sync", metav1.GetOptions{})
		require.NoError(t, err)
		ws := &v1alpha1.Workspace{}
		require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ws))
		return ws
	}
	view := func() api.Workspace {
		t.Helper()
		return ok[api.WorkspaceDetail](t, f.call(t, f.member, "get_workspace", map[string]any{"workspace": "sync"})).Workspace
	}
	jobs := func() []batchv1.Job {
		t.Helper()
		list, err := core.BatchV1().Jobs(wsNamespace).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		return list.Items
	}
	claim := func() *corev1.PersistentVolumeClaim {
		t.Helper()
		pvc, err := core.CoreV1().PersistentVolumeClaims(wsNamespace).Get(ctx, "workspace-sync", metav1.GetOptions{})
		require.NoError(t, err)
		return pvc
	}
	// bind marks the claim bound, the provisioner's work, of which envtest
	// runs none: the API server lets only a bound claim's request grow.
	bind := func() {
		t.Helper()
		pvc := claim()
		pvc.Status.Phase = corev1.ClaimBound
		_, err := core.CoreV1().PersistentVolumeClaims(wsNamespace).UpdateStatus(ctx, pvc, metav1.UpdateOptions{})
		require.NoError(t, err)
	}
	selection := func() map[string]time.Time {
		t.Helper()
		cm, err := core.CoreV1().ConfigMaps(wsNamespace).Get(ctx, "workspace-sync-selection", metav1.GetOptions{})
		require.NoError(t, err)
		var sel mirror.Selection
		require.NoError(t, json.Unmarshal([]byte(cm.Data["selection.json"]), &sel))
		out := map[string]time.Time{}
		for _, r := range sel.Repositories {
			out[r.Owner+"/"+r.Name] = r.PushedAt
		}
		return out
	}
	// running waits for a sync Job to start and returns the Workspace.
	running := func(trigger v1alpha1.SyncTrigger) *v1alpha1.Workspace {
		t.Helper()
		var ws *v1alpha1.Workspace
		require.Eventually(t, func() bool {
			ws = get()
			return ws.Status.ActiveSync != nil && ws.Status.ActiveSync.Trigger == trigger
		}, 30*time.Second, 200*time.Millisecond, "a sync Job started by %s", trigger)
		return ws
	}
	// complete ends the active Job as the Job controller would, with the
	// manifest the sync wrote, and waits for the controller to record it.
	complete := func(ws *v1alpha1.Workspace, at time.Time, entries ...mirror.Entry) *v1alpha1.Workspace {
		t.Helper()
		m := mirror.Manifest{SyncedAt: at, Repositories: entries}
		raw, err := m.JSON()
		require.NoError(t, err)
		require.NoError(t, kube.WriteConfigMap(ctx, core, wsNamespace, controller.ManifestName(ws), map[string]string{mirror.ManifestFile: string(raw)}))
		job, err := core.BatchV1().Jobs(wsNamespace).Get(ctx, ws.Status.ActiveSync.Job, metav1.GetOptions{})
		require.NoError(t, err)
		now := metav1.NewTime(at)
		job.Status.StartTime, job.Status.CompletionTime = &now, &now
		job.Status.Conditions = append(job.Status.Conditions,
			batchv1.JobCondition{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
			batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now})
		_, err = core.BatchV1().Jobs(wsNamespace).UpdateStatus(ctx, job, metav1.UpdateOptions{})
		require.NoError(t, err)
		var got *v1alpha1.Workspace
		require.Eventually(t, func() bool {
			got = get()
			return got.Status.ActiveSync == nil && got.Status.LastSync != nil && got.Status.LastSync.Job == job.Name
		}, 30*time.Second, 200*time.Millisecond, "the cycle of %s ends", job.Name)
		return got
	}
	mirrored := func(name string, sizeBytes int64, pushed time.Time) mirror.Entry {
		return mirror.Entry{Provider: "github", Owner: "acme", Name: name, Path: "mirrors/acme/" + name + ".git",
			DefaultBranch: "main", Head: "abc", PushedAt: pushed, SizeBytes: sizeBytes}
	}
	pushed := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	t.Run("a new workspace is claimed per the sizing rule and its Job started", func(t *testing.T) {
		// repo-001 and repo-002 by name, and whatever carries the topic.
		created := ok[api.Workspace](t, f.call(t, f.member, "create_workspace", map[string]any{"name": "sync", "organization": "acme",
			"sources": []any{map[string]any{"provider": "github", "owner": "acme", "repositories": []string{"repo-001", "repo-002"},
				"filters": map[string]any{"topics": map[string]any{"names": []string{"sync-envtest"}}}}}}))
		assert.Equal(t, "sync", created.Name)

		ws := running(v1alpha1.TriggerChange)
		assert.Len(t, jobs(), 1)
		assert.Equal(t, ws.Status.ActiveSync.Job, jobs()[0].Name)
		sel := selection()
		assert.Len(t, sel, 2)
		assert.Equal(t, pushed.Add(1*time.Minute), sel["acme/repo-001"], "the provider's push time: nothing is mirrored yet")
		// 1.5 × 2 MiB need; the headroom floor of 4Gi wins; rounded up.
		assert.Equal(t, "5Gi", claim().Spec.Resources.Requests.Storage().String())
		assert.Equal(t, "5Gi", ws.Status.VolumeSize.String())
		bind()
		cond := meta.FindStatusCondition(ws.Status.Conditions, v1alpha1.ConditionSynced)
		require.NotNil(t, cond)
		assert.Equal(t, v1alpha1.ReasonSyncing, cond.Reason)
		v := view()
		require.NotNil(t, v.ActiveSync)
		assert.Equal(t, ws.Status.ActiveSync.Job, v.ActiveSync.Job)
		assert.Equal(t, "5Gi", v.VolumeSize)
	})

	t.Run("the measured mirrors grow the volume", func(t *testing.T) {
		ws := get()
		got := complete(ws, clock.Now(), mirrored("repo-001", 2<<30, pushed.Add(time.Minute)), mirrored("repo-002", 3<<30, pushed.Add(2*time.Minute)))
		assert.Equal(t, v1alpha1.SyncSucceeded, got.Status.LastSync.Result)
		assert.Equal(t, v1alpha1.TriggerChange, got.Status.LastSync.Trigger)
		assert.Equal(t, []string{"acme/repo-001", "acme/repo-002"}, got.Status.Repositories)
		assert.Equal(t, "5Gi", got.Status.NeededSize.String())
		// 5Gi need plus max(4Gi, 2.5Gi): grown after the sync measured it.
		assert.Equal(t, "9Gi", got.Status.VolumeSize.String())
		assert.Equal(t, "9Gi", claim().Spec.Resources.Requests.Storage().String())
		assert.Equal(t, "Synced", meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionSynced).Reason)
	})

	t.Run("a day passes without a cycle; a request with nothing changed starts no Job", func(t *testing.T) {
		before := get()
		clock.Set(before.Status.LastSync.Time.Add(24 * time.Hour))
		f.call(t, f.member, "get_workspace", map[string]any{"workspace": "sync"})
		assert.Equal(t, before.Status.LastSync.Time.UTC(), get().Status.LastSync.Time.UTC(), "nothing syncs on a schedule")
		ok[api.SyncResult](t, f.call(t, f.member, "sync_workspace", map[string]any{"workspace": "sync"}))
		var got *v1alpha1.Workspace
		require.Eventually(t, func() bool {
			got = get()
			return got.Status.LastSync != nil && got.Status.LastSync.Trigger == v1alpha1.TriggerRequest
		}, 30*time.Second, 200*time.Millisecond, "the requested cycle runs")
		assert.Equal(t, v1alpha1.SyncNoChange, got.Status.LastSync.Result)
		assert.Empty(t, got.Status.LastSync.Job)
		assert.Equal(t, clock.Now().UTC(), got.Status.LastSync.Time.UTC())
		assert.Len(t, jobs(), 1, "no second Job")
		assert.Equal(t, v1alpha1.ReasonNoChange, meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionSynced).Reason)
	})

	t.Run("a push and sync_workspace start a Job for the changed repository alone", func(t *testing.T) {
		later := pushed.Add(48 * time.Hour)
		f.gh.Put(provider.Item{Owner: "acme", Name: "repo-001", Language: "TypeScript", SizeKiB: 1024, LastChange: later})
		res := ok[api.SyncResult](t, f.call(t, f.member, "sync_workspace", map[string]any{"workspace": "sync"}))
		assert.Equal(t, "alice@acme.test", res.RequestedBy)
		assert.Nil(t, res.ActiveSync)

		ws := running(v1alpha1.TriggerRequest)
		assert.Equal(t, []string{res.RequestID}, ws.Status.ActiveSync.Requests, "the Job serves the request")
		require.Eventually(t, func() bool {
			ws = get()
			return ws.Annotations[v1alpha1.AnnotationSyncRequested] == ""
		}, 30*time.Second, 200*time.Millisecond, "the request is taken once the status names it")
		assert.Len(t, jobs(), 2)
		sel := selection()
		assert.Equal(t, later, sel["acme/repo-001"], "the push")
		assert.Equal(t, pushed.Add(2*time.Minute), sel["acme/repo-002"], "unchanged: the sync does not contact it")
		assert.Nil(t, view().SyncRequest)

		got := complete(ws, clock.Now(), mirrored("repo-001", 2<<30, later), mirrored("repo-002", 3<<30, pushed.Add(2*time.Minute)))
		assert.Equal(t, v1alpha1.TriggerRequest, got.Status.LastSync.Trigger)
		assert.Equal(t, []string{res.RequestID}, got.Status.LastSync.Requests, "the cycle names the request it served")
		assert.Equal(t, v1alpha1.SyncSucceeded, got.Status.LastSync.Result)
		assert.Equal(t, "9Gi", got.Status.VolumeSize.String(), "the same need: nothing to grow")
	})

	t.Run("a repository gaining the topic joins at the next cycle, the volume grown ahead", func(t *testing.T) {
		// A gibibyte reported: 1.5Gi estimated until it is mirrored.
		f.gh.Put(provider.Item{Owner: "acme", Name: "repo-003", Language: "Go", SizeKiB: 1 << 20, LastChange: pushed, Topics: []string{"sync-envtest"}})
		ok[api.SyncResult](t, f.call(t, f.member, "sync_workspace", map[string]any{"workspace": "sync"}))

		ws := running(v1alpha1.TriggerRequest)
		assert.Len(t, jobs(), 3)
		sel := selection()
		assert.Len(t, sel, 3)
		assert.Equal(t, pushed, sel["acme/repo-003"])
		// 5Gi measured plus 1.5Gi estimated plus 4Gi, rounded up: grown
		// before the added repository is mirrored.
		assert.Equal(t, "11Gi", claim().Spec.Resources.Requests.Storage().String())
		assert.Equal(t, "11Gi", ws.Status.VolumeSize.String())

		// Mirrored, it measures 1Gi: the need is 6Gi, the share stays 11Gi,
		// never shrunk.
		got := complete(ws, clock.Now(), mirrored("repo-001", 2<<30, pushed.Add(48*time.Hour)),
			mirrored("repo-002", 3<<30, pushed.Add(2*time.Minute)), mirrored("repo-003", 1<<30, pushed))
		assert.Equal(t, []string{"acme/repo-001", "acme/repo-002", "acme/repo-003"}, got.Status.Repositories)
		assert.Equal(t, "6Gi", got.Status.NeededSize.String())
		assert.Equal(t, "11Gi", got.Status.VolumeSize.String())
		assert.Equal(t, "11Gi", claim().Spec.Resources.Requests.Storage().String())
		assert.Equal(t, 3, view().Repositories)
	})
}
