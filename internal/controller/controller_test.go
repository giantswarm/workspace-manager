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
	"golang.org/x/oauth2"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

const (
	testNamespace = "kagent"
	// testClass is elastic (EFS): no size is provisioned.
	testClass = "efs-rwx"
	// testProvisionedClass provisions a size and expands online (Azure Files).
	testProvisionedClass = "azurefile-nfs"
	testProvider         = "github"
	testOwner            = "acme"
)

// testNow is a Wednesday at noon UTC: the nightly sync is due at 03:00 the
// next day.
var testNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// stubKind is a provider kind whose sync credential is a fixed token: the
// cycle's credentials Secret is built from it.
type stubKind struct{}

func (stubKind) List(context.Context, oauth2.TokenSource, string) ([]provider.Item, error) {
	return nil, errors.New("the controller lists through Listings")
}

func (stubKind) Readable(context.Context, oauth2.TokenSource, string, []string) ([]provider.Item, error) {
	return nil, errors.New("not used")
}

func (stubKind) SyncCredential(context.Context, provider.Secrets, string) (oauth2.TokenSource, error) {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "stub-installation-token"}), nil
}

func (stubKind) SignIn() provider.SignIn { return provider.SignIn{} }

func (stubKind) Hosts() provider.Hosts {
	return provider.Hosts{Git: []provider.Host{{Name: "code.test", Scheme: provider.SchemeBasic, User: "x-access-token"}}}
}

func (stubKind) SecretRefs() []provider.SecretRef { return nil }

// listings is a Listings over fixed items per provider/owner, counting its
// calls: the no-op check must cost one listing and nothing else.
type listings struct {
	mu    sync.Mutex
	items map[string][]provider.Item
	calls int
	err   error
}

func (l *listings) Resolve(_ context.Context, sources []v1alpha1.Source) (*workspace.Selection, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	sel := &workspace.Selection{}
	for _, s := range sources {
		for _, it := range provider.Select(l.items[s.Provider+"/"+s.Owner], workspace.SourceOf(s)) {
			sel.Items = append(sel.Items, workspace.Selected{Provider: s.Provider, Item: it, Why: []string{"name"}})
		}
	}
	return sel, nil
}

func (l *listings) put(it provider.Item) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := testProvider + "/" + it.Owner
	for i := range l.items[key] {
		if l.items[key][i].Name == it.Name {
			l.items[key][i] = it
			return
		}
	}
	l.items[key] = append(l.items[key], it)
}

func (l *listings) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// item is acme's repository name as the provider lists it.
func item(name string, pushed time.Time, sizeKiB int64) provider.Item {
	return provider.Item{Owner: testOwner, Name: name, LastChange: pushed, SizeKiB: sizeKiB,
		CloneURL: "https://code.test/" + testOwner + "/" + name + ".git", DefaultBranch: "main"}
}

// entry is a mirror the manifest records for an item.
func entry(it provider.Item, sizeBytes int64) mirror.Entry {
	return mirror.Entry{Provider: testProvider, Owner: it.Owner, Name: it.Name, Path: "mirrors/" + it.Owner + "/" + it.Name + ".git",
		CloneURL: it.CloneURL, DefaultBranch: it.DefaultBranch, Head: "abc", PushedAt: it.LastChange, SizeBytes: sizeBytes}
}

// workspace is a Workspace as the API server serves it: typed, with a UID,
// selecting acme's api and web by name.
func workspaceNamed(name string) *v1alpha1.Workspace {
	return &v1alpha1.Workspace{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Workspace"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: types.UID("uid-" + name), Generation: 3},
		Spec: v1alpha1.WorkspaceSpec{
			Organization: testOwner,
			Sources:      []v1alpha1.Source{{Provider: testProvider, Owner: testOwner, Repositories: []string{"api", "web"}}},
		},
	}
}

func toUnstructured(t *testing.T, ws *v1alpha1.Workspace) *unstructured.Unstructured {
	t.Helper()
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ws)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: obj}
}

// fixture is a controller on fake clients, a stopped clock and fixed
// listings.
type fixture struct {
	t        *testing.T
	c        *Controller
	dyn      *dynamicfake.FakeDynamicClient
	core     *fake.Clientset
	listings *listings
	mu       sync.Mutex
	now      time.Time
}

// newFixture builds the controller on class with the given Workspaces; the
// listings hold api and web, pushed a day before testNow, 1 MiB reported
// each.
func newFixture(t *testing.T, class string, wss ...*v1alpha1.Workspace) *fixture {
	t.Helper()
	var initial []runtime.Object
	for _, ws := range wss {
		initial = append(initial, toUnstructured(t, ws))
	}
	// An empty scheme: the fake keeps the objects unstructured, as the API
	// server serves them to the dynamic client.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{v1alpha1.WorkspaceResource: "WorkspaceList"}, initial...)
	core := fake.NewClientset(
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: testClass}, Provisioner: "efs.csi.aws.com"},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: testProvisionedClass}, Provisioner: "file.csi.azure.com", AllowVolumeExpansion: ptr.To(true)},
	)
	pushed := testNow.Add(-24 * time.Hour)
	f := &fixture{t: t, dyn: dyn, core: core, now: testNow,
		listings: &listings{items: map[string][]provider.Item{testProvider + "/" + testOwner: {item("api", pushed, 1024), item("web", pushed, 1024)}}}}
	f.c = New(Config{
		Dynamic: dyn, Core: core, Namespace: testNamespace, StorageClass: class, Resync: time.Hour,
		Listings:           f.listings,
		Instances:          []provider.Instance{{Name: testProvider, KindName: "stub", Kind: stubKind{}}},
		SyncServiceAccount: "workspace-manager-sync", SyncImage: "gsoci.azurecr.io/giantswarm/workspace-manager:test",
		Now: f.clock,
	})
	return f
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fixture) workspace(name string) *v1alpha1.Workspace {
	f.t.Helper()
	u, err := f.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(f.t, err)
	ws, err := fromUnstructured(u)
	require.NoError(f.t, err)
	return ws
}

// updateSpec changes the stored Workspace's spec as a member would, with the
// new generation the API server would stamp.
func (f *fixture) updateSpec(name string, mutate func(*v1alpha1.Workspace)) {
	f.t.Helper()
	ws := f.workspace(name)
	mutate(ws)
	ws.Generation++
	_, err := f.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Update(context.Background(), toUnstructured(f.t, ws), metav1.UpdateOptions{})
	require.NoError(f.t, err)
}

// reconcile runs Reconcile on the stored Workspace, as the loop does.
// requestSync records a sync request as sync_workspace and
// authorize_workspace do.
func (f *fixture) requestSync(name string) {
	f.t.Helper()
	patch := `{"metadata":{"annotations":{"` + v1alpha1.AnnotationSyncRequested + `":"` + f.clock().Format(time.RFC3339) + `","` + v1alpha1.AnnotationSyncRequestedBy + `":"alice@acme.test"}}}`
	_, err := f.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Patch(context.Background(), name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	require.NoError(f.t, err)
}

func (f *fixture) reconcile(name string) time.Duration {
	f.t.Helper()
	requeue, err := f.c.Reconcile(context.Background(), f.workspace(name))
	require.NoError(f.t, err)
	return requeue
}

func (f *fixture) claims() []corev1.PersistentVolumeClaim {
	f.t.Helper()
	list, err := f.core.CoreV1().PersistentVolumeClaims(testNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(f.t, err)
	return list.Items
}

func (f *fixture) jobs() []batchv1.Job {
	f.t.Helper()
	list, err := f.core.BatchV1().Jobs(testNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(f.t, err)
	return list.Items
}

func (f *fixture) configMap(name string) *corev1.ConfigMap {
	f.t.Helper()
	cm, err := f.core.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(f.t, err)
	return cm
}

// selection is the sync Job's input as the ConfigMap carries it.
func (f *fixture) selection(ws *v1alpha1.Workspace) mirror.Selection {
	f.t.Helper()
	var sel mirror.Selection
	require.NoError(f.t, json.Unmarshal([]byte(f.configMap(SelectionName(ws)).Data[selectionFile]), &sel))
	return sel
}

// writeManifest records entries as the sync Job's result and names it in
// the status, as a finished cycle leaves it.
func (f *fixture) writeManifest(ws *v1alpha1.Workspace, syncedAt time.Time, entries ...mirror.Entry) {
	f.t.Helper()
	m := mirror.Manifest{SyncedAt: syncedAt, Repositories: entries}
	raw, err := m.JSON()
	require.NoError(f.t, err)
	cms := f.core.CoreV1().ConfigMaps(testNamespace)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ManifestName(ws), Namespace: testNamespace}, Data: map[string]string{mirror.ManifestFile: string(raw)}}
	if existing, err := cms.Get(context.Background(), cm.Name, metav1.GetOptions{}); err == nil {
		existing.Data = cm.Data
		_, err = cms.Update(context.Background(), existing, metav1.UpdateOptions{})
		require.NoError(f.t, err)
	} else {
		_, err = cms.Create(context.Background(), cm, metav1.CreateOptions{})
		require.NoError(f.t, err)
	}
}

// finishJob ends the Workspace's active sync Job with the condition, as the
// Job controller would.
func (f *fixture) finishJob(ws *v1alpha1.Workspace, condition batchv1.JobConditionType, at time.Time) {
	f.t.Helper()
	require.NotNil(f.t, ws.Status.ActiveSync, "a sync Job runs")
	jobs := f.core.BatchV1().Jobs(testNamespace)
	job, err := jobs.Get(context.Background(), ws.Status.ActiveSync.Job, metav1.GetOptions{})
	require.NoError(f.t, err)
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: condition, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)})
	_, err = jobs.UpdateStatus(context.Background(), job, metav1.UpdateOptions{})
	require.NoError(f.t, err)
}

// synced is the Workspace's Synced condition.
func syncedCondition(t *testing.T, ws *v1alpha1.Workspace) *metav1.Condition {
	t.Helper()
	cond := meta.FindStatusCondition(ws.Status.Conditions, v1alpha1.ConditionSynced)
	require.NotNil(t, cond, "a Synced condition")
	return cond
}

// firstCycle runs a Workspace's first cycle to its Job, as a new Workspace
// goes: claimed, listed, the Job started.
func (f *fixture) firstCycle(name string) *v1alpha1.Workspace {
	f.t.Helper()
	requeue := f.reconcile(name)
	assert.Zero(f.t, requeue, "a running Job wakes the controller by itself")
	ws := f.workspace(name)
	require.NotNil(f.t, ws.Status.ActiveSync)
	return ws
}

// completeCycle ends the active Job successfully with the manifest the sync
// would write, and reconciles the outcome.
func (f *fixture) completeCycle(ws *v1alpha1.Workspace, at time.Time, entries ...mirror.Entry) *v1alpha1.Workspace {
	f.t.Helper()
	f.writeManifest(ws, at, entries...)
	f.finishJob(ws, batchv1.JobComplete, at)
	f.reconcile(ws.Name)
	return f.workspace(ws.Name)
}

// TestFirstCycleClaimsAndStartsTheJob: a new Workspace is listed once, its
// volume claimed (ReadWriteMany from the class, owned by the Workspace, the
// nominal size on an elastic class) and its sync Job started with the
// selection and the credential; the status says so.
func TestFirstCycleClaimsAndStartsTheJob(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")

	assert.Equal(t, 1, f.listings.count(), "one listing per owner")
	claims := f.claims()
	require.Len(t, claims, 1)
	pvc := claims[0]
	assert.Equal(t, "workspace-platform", pvc.Name)
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, pvc.Spec.AccessModes)
	assert.Equal(t, testClass, ptr.Deref(pvc.Spec.StorageClassName, ""))
	assert.True(t, NominalSize.Equal(pvc.Spec.Resources.Requests[corev1.ResourceStorage]), "an elastic class gets the nominal request")
	require.Len(t, pvc.OwnerReferences, 1)
	assert.Equal(t, ws.UID, pvc.OwnerReferences[0].UID)
	assert.True(t, ptr.Deref(pvc.OwnerReferences[0].Controller, false))

	jobs := f.jobs()
	require.Len(t, jobs, 1)
	job := jobs[0]
	assert.Equal(t, ws.Status.ActiveSync.Job, job.Name)
	assert.Equal(t, "platform", job.Labels[LabelWorkspace])
	assert.NotEmpty(t, job.Labels[LabelSync])
	assert.Equal(t, ws.UID, job.OwnerReferences[0].UID)
	pod := job.Spec.Template.Spec
	assert.Equal(t, "workspace-manager-sync", pod.ServiceAccountName)
	assert.Equal(t, corev1.RestartPolicyNever, pod.RestartPolicy)
	require.Len(t, pod.Containers, 1)
	c := pod.Containers[0]
	assert.Equal(t, SyncContainer, c.Name)
	assert.Equal(t, "gsoci.azurecr.io/giantswarm/workspace-manager:test", c.Image)
	assert.Equal(t, []string{"sync"}, c.Args)
	assert.Contains(t, c.Env, corev1.EnvVar{Name: "WORKSPACE_MANAGER_SYNC_RESULT_CONFIGMAP", Value: "workspace-platform-manifest"})
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	assert.Equal(t, map[string]string{"workspace": volumeMountPath, "selection": selectionMountPath, "credentials": credentialsMountPath}, mounts)
	volumes := map[string]corev1.VolumeSource{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v.VolumeSource
	}
	assert.Equal(t, "workspace-platform", volumes["workspace"].PersistentVolumeClaim.ClaimName)
	assert.Equal(t, SelectionName(ws), volumes["selection"].ConfigMap.Name)
	assert.Equal(t, CredentialsName(ws), volumes["credentials"].Secret.SecretName)
	assert.Equal(t, []corev1.KeyToPath{{Key: "0.username", Path: "github/acme/username"}, {Key: "0.token", Path: "github/acme/token"}}, volumes["credentials"].Secret.Items)

	sel := f.selection(ws)
	require.Len(t, sel.Repositories, 2)
	assert.Equal(t, mirror.Repository{Provider: testProvider, Owner: testOwner, Name: "api", CloneURL: "https://code.test/acme/api.git",
		DefaultBranch: "main", PushedAt: testNow.Add(-24 * time.Hour)}, sel.Repositories[0])
	secret, err := f.core.CoreV1().Secrets(testNamespace).Get(context.Background(), CredentialsName(ws), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string][]byte{"0.username": []byte("x-access-token"), "0.token": []byte("stub-installation-token")}, secret.Data)
	assert.Equal(t, ws.UID, secret.OwnerReferences[0].UID, "the credential goes with the Workspace")
	manifest := f.configMap(ManifestName(ws))
	assert.Equal(t, ws.UID, manifest.OwnerReferences[0].UID, "the manifest is made ahead, owned by the Workspace")

	assert.Equal(t, ws.Generation, ws.Status.ObservedGeneration)
	assert.Equal(t, v1alpha1.TriggerChange, ws.Status.ActiveSync.Trigger)
	assert.Equal(t, testNow, ws.Status.ActiveSync.StartedAt.UTC())
	assert.Nil(t, ws.Status.LastSync)
	assert.Equal(t, ManifestName(ws), ws.Status.Manifest)
	require.NotNil(t, ws.Status.Volume)
	assert.Equal(t, "workspace-platform", ws.Status.Volume.ClaimName)
	assert.Nil(t, ws.Status.VolumeSize, "nothing is sized on an elastic class")
	assert.Equal(t, "3Mi", ws.Status.NeededSize.String(), "1.5 × the reported sizes, nothing mirrored yet")
	cond := syncedCondition(t, ws)
	assert.Equal(t, metav1.ConditionUnknown, cond.Status)
	assert.Equal(t, v1alpha1.ReasonSyncing, cond.Reason)
	assert.Contains(t, cond.Message, job.Name)
	claimed := meta.FindStatusCondition(ws.Status.Conditions, v1alpha1.ConditionVolumeClaimed)
	require.NotNil(t, claimed)
	assert.Equal(t, metav1.ConditionTrue, claimed.Status)

	// While the Job runs, a reconcile changes nothing: no second Job, no
	// second listing.
	assert.Zero(t, f.reconcile("platform"))
	assert.Len(t, f.jobs(), 1)
	assert.Equal(t, 1, f.listings.count())
}

// TestCycleEndsWithTheJob: once the Job completed, the cycle's outcome is
// the manifest: the repositories, the measured need, the credential
// emptied; nothing is due after it.
func TestCycleEndsWithTheJob(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")
	ended := testNow.Add(2 * time.Minute)
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	f.advance(2 * time.Minute)

	got := f.completeCycle(ws, ended, entry(api, 2<<20), entry(web, 3<<20))

	assert.Nil(t, got.Status.ActiveSync)
	require.NotNil(t, got.Status.LastSync)
	assert.Equal(t, v1alpha1.SyncSucceeded, got.Status.LastSync.Result)
	assert.Equal(t, v1alpha1.TriggerChange, got.Status.LastSync.Trigger)
	assert.Equal(t, ws.Status.ActiveSync.Job, got.Status.LastSync.Job)
	assert.Equal(t, ended, got.Status.LastSync.Time.UTC())
	assert.Equal(t, []string{"acme/api", "acme/web"}, got.Status.Repositories)
	assert.Equal(t, "5Mi", got.Status.NeededSize.String(), "the measured mirrors")
	cond := syncedCondition(t, got)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, v1alpha1.ReasonSynced, cond.Reason)
	secret, err := f.core.CoreV1().Secrets(testNamespace).Get(context.Background(), CredentialsName(ws), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, secret.Data, "the credential lives only while the Job runs")

	// Nothing is due: no schedule, no timer.
	assert.Zero(t, f.reconcile("platform"))
	assert.Equal(t, 1, f.listings.count(), "no listing while nothing is due")
}

// TestNothingSyncsOnASchedule: a synced workspace with nothing requested
// and nothing changed lists nothing, however long it waits; a request
// (sync_workspace, or authorize_workspace past the refresh threshold)
// starts the next cycle.
func TestNothingSyncsOnASchedule(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	f.completeCycle(ws, testNow, entry(api, 2<<20), entry(web, 3<<20))

	for range 14 {
		f.advance(12 * time.Hour)
		assert.Zero(t, f.reconcile("platform"), "no timer")
	}
	assert.Equal(t, 1, f.listings.count(), "a week passes without a listing")

	f.requestSync("platform")
	f.reconcile("platform")
	got := f.workspace("platform")
	assert.Equal(t, 2, f.listings.count())
	assert.Equal(t, v1alpha1.TriggerRequest, got.Status.LastSync.Trigger)
}

// TestAStoredSizeIsRecordedForAWorkspaceSyncedWithoutOne: a Workspace whose
// last sync recorded no stored size has it measured from its manifest and
// session directories at the next reconcile, with no cycle due.
func TestAStoredSizeIsRecordedForAWorkspaceSyncedWithoutOne(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	got := f.completeCycle(ws, testNow, entry(api, 2<<20), entry(web, 3<<20))
	assert.Equal(t, "5Mi", got.Status.StoredSize.String())

	got.Status.StoredSize = nil
	_, err := f.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).UpdateStatus(context.Background(), toUnstructured(t, got), metav1.UpdateOptions{})
	require.NoError(t, err)
	listings := f.listings.count()

	f.reconcile("platform")
	got = f.workspace("platform")
	require.NotNil(t, got.Status.StoredSize)
	assert.Equal(t, "5Mi", got.Status.StoredSize.String(), "the manifest's mirrors")
	assert.Equal(t, listings, f.listings.count(), "no cycle ran")
}

// TestNoChangeCostsOneListing: a requested cycle whose selection equals the
// manifest, with no newer push, ends with NoChange after one listing: no
// Job, no fetch, nothing due after it.
func TestNoChangeCostsOneListing(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	f.completeCycle(ws, testNow, entry(api, 2<<20), entry(web, 3<<20))
	require.Equal(t, 1, f.listings.count())

	// A Session starts past the refresh threshold.
	f.advance(2 * time.Hour)
	f.requestSync("platform")
	requeue := f.reconcile("platform")

	got := f.workspace("platform")
	assert.Equal(t, 2, f.listings.count(), "the cycle lists once")
	assert.Len(t, f.jobs(), 1, "no second Job")
	assert.Nil(t, got.Status.ActiveSync)
	assert.Equal(t, v1alpha1.SyncNoChange, got.Status.LastSync.Result)
	assert.Equal(t, v1alpha1.TriggerRequest, got.Status.LastSync.Trigger)
	assert.Empty(t, got.Status.LastSync.Job)
	assert.Equal(t, f.clock(), got.Status.LastSync.Time.UTC())
	assert.Zero(t, requeue, "nothing due")
	cond := syncedCondition(t, got)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, v1alpha1.ReasonNoChange, cond.Reason)
	assert.Equal(t, []string{"acme/api", "acme/web"}, got.Status.Repositories, "kept from the last manifest")
}

// TestAPushStartsAJobForTheChangedRepository: a repository pushed to since
// the manifest makes the next cycle start a Job whose selection carries the
// newer push time for it alone; the other keeps the manifest's, so the sync
// fetches only the changed one.
func TestAPushStartsAJobForTheChangedRepository(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	f.completeCycle(ws, testNow, entry(api, 2<<20), entry(web, 3<<20))

	pushed := f.clock().Add(time.Hour)
	f.listings.put(item("web", pushed, 1024))
	f.advance(2 * time.Hour)
	f.requestSync("platform")
	assert.Zero(t, f.reconcile("platform"))

	got := f.workspace("platform")
	require.NotNil(t, got.Status.ActiveSync, "a Job for the push")
	assert.Equal(t, v1alpha1.TriggerRequest, got.Status.ActiveSync.Trigger)
	assert.Len(t, f.jobs(), 2)
	sel := f.selection(got)
	require.Len(t, sel.Repositories, 2)
	assert.Equal(t, api.LastChange, sel.Repositories[0].PushedAt, "api as the manifest has it")
	assert.Equal(t, pushed, sel.Repositories[1].PushedAt, "web's newer push")
}

// TestARepositoryJoinsAtTheNextCycle: a repository the selection gains (a
// filter it now matches) is in the next cycle's Job, estimated into the
// need; one it loses is left to the sync to mark dropped.
func TestARepositoryJoinsAtTheNextCycle(t *testing.T) {
	ws := workspaceNamed("platform")
	ws.Spec.Sources = []v1alpha1.Source{{Provider: testProvider, Owner: testOwner, Filters: &v1alpha1.Filters{Topics: &v1alpha1.TopicFilter{Names: []string{"mcp"}}}}}
	f := newFixture(t, testClass, ws)
	api := item("api", testNow.Add(-24*time.Hour), 1024)
	api.Topics = []string{"mcp"}
	f.listings.put(api)
	ws = f.firstCycle("platform")
	require.Len(t, f.selection(ws).Repositories, 1, "api alone carries the topic")
	f.completeCycle(ws, testNow, entry(api, 2<<20))

	web := item("web", testNow.Add(-24*time.Hour), 4096)
	web.Topics = []string{"mcp"}
	f.listings.put(web)
	f.advance(2 * time.Hour)
	f.requestSync("platform")
	f.reconcile("platform")

	got := f.workspace("platform")
	require.NotNil(t, got.Status.ActiveSync)
	sel := f.selection(got)
	require.Len(t, sel.Repositories, 2)
	assert.Equal(t, "web", sel.Repositories[1].Name)
	assert.Equal(t, "8Mi", got.Status.NeededSize.String(), "api's 2 MiB measured, web's 4 MiB reported × 1.5")
}

// TestSyncRequestStartsACycleNow: the annotations sync_workspace sets start
// a cycle at once, are removed with it, and the cycle is recorded as
// requested; one made while a Job runs is served after it.
func TestSyncRequestStartsACycleNow(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	f.completeCycle(ws, testNow, entry(api, 2<<20), entry(web, 3<<20))

	request := func() { f.requestSync("platform") }
	f.advance(time.Hour)
	request()
	f.reconcile("platform")

	got := f.workspace("platform")
	assert.Equal(t, 2, f.listings.count(), "the request lists at once")
	assert.Empty(t, got.Annotations[v1alpha1.AnnotationSyncRequested], "the request is taken")
	assert.Empty(t, got.Annotations[v1alpha1.AnnotationSyncRequestedBy])
	assert.Equal(t, v1alpha1.SyncNoChange, got.Status.LastSync.Result, "nothing changed: no fetch for the request either")
	assert.Equal(t, v1alpha1.TriggerRequest, got.Status.LastSync.Trigger)

	// A push, a request: the Job runs; a second request during it waits
	// for the Job and starts the next cycle right after.
	f.listings.put(item("api", f.clock(), 1024))
	request()
	f.reconcile("platform")
	got = f.workspace("platform")
	require.NotNil(t, got.Status.ActiveSync)
	assert.Equal(t, v1alpha1.TriggerRequest, got.Status.ActiveSync.Trigger)
	request()
	f.reconcile("platform")
	assert.NotEmpty(t, f.workspace("platform").Annotations[v1alpha1.AnnotationSyncRequested], "kept while the Job runs")
	f.completeCycle(got, f.clock(), entry(f.listings.items[testProvider+"/"+testOwner][0], 2<<20), entry(web, 3<<20))
	got = f.workspace("platform")
	assert.Empty(t, got.Annotations[v1alpha1.AnnotationSyncRequested], "served after the Job")
	assert.Equal(t, v1alpha1.TriggerRequest, got.Status.LastSync.Trigger)
	assert.Equal(t, v1alpha1.SyncNoChange, got.Status.LastSync.Result)
	assert.Equal(t, 4, f.listings.count())
}

// TestASpecChangeStartsACycle: a new generation starts a cycle at once.
func TestASpecChangeStartsACycle(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	f.completeCycle(ws, testNow, entry(api, 2<<20), entry(web, 3<<20))

	f.updateSpec("platform", func(ws *v1alpha1.Workspace) { ws.Spec.Sources[0].Repositories = []string{"api"} })

	f.reconcile("platform")
	got := f.workspace("platform")
	require.NotNil(t, got.Status.ActiveSync, "web left the selection: the sync marks it dropped")
	assert.Equal(t, v1alpha1.TriggerChange, got.Status.ActiveSync.Trigger)
	assert.Equal(t, int64(4), got.Status.ObservedGeneration)
	assert.Len(t, f.selection(got).Repositories, 1)
}

// TestAFailedListingIsRetriedAtTheResync: a provider's listing error ends
// the cycle as failed with its message; the next try waits the resync, and
// a request does not.
func TestAFailedListingIsRetriedAtTheResync(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	f.listings.err = errors.New("list acme on github: 503 Service Unavailable")

	requeue := f.reconcile("platform")
	got := f.workspace("platform")
	assert.Equal(t, time.Hour, requeue, "the resync")
	assert.Empty(t, f.jobs())
	require.NotNil(t, got.Status.LastSync)
	assert.Equal(t, v1alpha1.SyncFailed, got.Status.LastSync.Result)
	assert.Equal(t, v1alpha1.TriggerChange, got.Status.LastSync.Trigger)
	assert.Contains(t, got.Status.LastSync.Message, "503")
	cond := syncedCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v1alpha1.ReasonListingFailed, cond.Reason)
	assert.Equal(t, ws3(got), got.Status.ObservedGeneration, "the change is observed: the retry is the resync's")

	f.advance(30 * time.Minute)
	requeue = f.reconcile("platform")
	assert.Equal(t, 1, f.listings.count(), "not before the resync")
	assert.Equal(t, 30*time.Minute, requeue)

	f.listings.err = nil
	f.advance(30 * time.Minute)
	f.reconcile("platform")
	got = f.workspace("platform")
	require.NotNil(t, got.Status.ActiveSync)
	assert.Equal(t, v1alpha1.TriggerRetry, got.Status.ActiveSync.Trigger)
}

func ws3(ws *v1alpha1.Workspace) int64 { return ws.Generation }

// TestAFailedJobKeepsTheManifest: a Job that failed ends the cycle with the
// error the sync wrote beside the manifest; the manifest and the
// repositories stay as they were, and the retry is the resync.
func TestAFailedJobKeepsTheManifest(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	f.completeCycle(ws, testNow, entry(api, 2<<20), entry(web, 3<<20))

	f.listings.put(item("web", f.clock().Add(time.Hour), 1024))
	f.advance(15 * time.Hour)
	f.requestSync("platform")
	f.reconcile("platform")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	cm := f.configMap(ManifestName(ws))
	cm.Data[mirror.ErrorKey] = "acme/web: git fetch: exit status 128"
	_, err := f.core.CoreV1().ConfigMaps(testNamespace).Update(context.Background(), cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	ended := f.clock().Add(time.Minute)
	f.finishJob(ws, batchv1.JobFailed, ended)
	requeue := f.reconcile("platform")

	got := f.workspace("platform")
	assert.Nil(t, got.Status.ActiveSync)
	assert.Equal(t, v1alpha1.SyncFailed, got.Status.LastSync.Result)
	assert.Equal(t, "acme/web: git fetch: exit status 128", got.Status.LastSync.Message)
	assert.Equal(t, ws.Status.ActiveSync.Job, got.Status.LastSync.Job)
	assert.Equal(t, []string{"acme/api", "acme/web"}, got.Status.Repositories, "the last manifest's")
	cond := syncedCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v1alpha1.ReasonSyncFailed, cond.Reason)
	assert.Equal(t, ended.Add(time.Hour).Sub(f.clock()), requeue)
}

// TestAJobGoneStillCounts: a Job deleted before its end was seen (its TTL)
// succeeded when its manifest is newer than its start, and failed otherwise.
func TestAJobGoneStillCounts(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	ws := f.firstCycle("platform")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	f.writeManifest(ws, testNow.Add(time.Minute), entry(api, 2<<20), entry(web, 3<<20))
	require.NoError(t, f.core.BatchV1().Jobs(testNamespace).Delete(context.Background(), ws.Status.ActiveSync.Job, metav1.DeleteOptions{}))
	f.advance(time.Hour)
	f.reconcile("platform")
	got := f.workspace("platform")
	assert.Equal(t, v1alpha1.SyncSucceeded, got.Status.LastSync.Result)
	assert.Contains(t, got.Status.LastSync.Message, "manifest stands")
	assert.Equal(t, []string{"acme/api", "acme/web"}, got.Status.Repositories)

	f.listings.put(item("api", f.clock(), 1024))
	f.advance(15 * time.Hour)
	f.requestSync("platform")
	f.reconcile("platform")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	require.NoError(t, f.core.BatchV1().Jobs(testNamespace).Delete(context.Background(), ws.Status.ActiveSync.Job, metav1.DeleteOptions{}))
	f.reconcile("platform")
	got = f.workspace("platform")
	assert.Equal(t, v1alpha1.SyncFailed, got.Status.LastSync.Result)
	assert.Contains(t, got.Status.LastSync.Message, "gone without a manifest")
}

// TestWithoutAStorageClassNothingSyncs: no class, no claim (never the
// cluster's default), no Job: both conditions say so, naming the flag and
// the chart value, and the cycle is tried again at the resync.
func TestWithoutAStorageClassNothingSyncs(t *testing.T) {
	f := newFixture(t, "", workspaceNamed("platform"))
	requeue := f.reconcile("platform")
	got := f.workspace("platform")
	assert.Equal(t, time.Hour, requeue)
	assert.Empty(t, f.claims())
	assert.Empty(t, f.jobs())
	assert.Nil(t, got.Status.Volume)
	claimed := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionVolumeClaimed)
	require.NotNil(t, claimed)
	assert.Equal(t, metav1.ConditionFalse, claimed.Status)
	assert.Equal(t, v1alpha1.ReasonStorageClassMissing, claimed.Reason)
	assert.Contains(t, claimed.Message, "--storage-class")
	assert.Contains(t, claimed.Message, "storage.storageClassName")
	cond := syncedCondition(t, got)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v1alpha1.ReasonVolumeMissing, cond.Reason)
	assert.Equal(t, v1alpha1.SyncFailed, got.Status.LastSync.Result)
}

// TestARefusedClaimIsRecorded: an API server error on the claim lands in
// the conditions and the cycle fails; the loop tries again at the resync.
func TestARefusedClaimIsRecorded(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	f.core.PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), "workspace-platform", nil)
	})
	requeue := f.reconcile("platform")
	got := f.workspace("platform")
	assert.Equal(t, time.Hour, requeue)
	claimed := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionVolumeClaimed)
	require.NotNil(t, claimed)
	assert.Equal(t, v1alpha1.ReasonVolumeClaimFailed, claimed.Reason)
	assert.Contains(t, claimed.Message, "forbidden")
	assert.Equal(t, v1alpha1.ReasonVolumeMissing, syncedCondition(t, got).Reason)
	assert.Empty(t, f.jobs())
}

// TestAProvisionedClassIsSizedAndGrown: on a class that provisions a size
// the claim is made at the formula's size for the estimate, grown before
// the mirrors are added when the estimate does not fit, and grown after
// the sync to the measured need plus the headroom; never shrunk.
func TestAProvisionedClassIsSizedAndGrown(t *testing.T) {
	f := newFixture(t, testProvisionedClass, workspaceNamed("platform"))
	// api 1 GiB and web 2 GiB reported: 4.5 GiB estimated, 4 GiB headroom.
	f.listings.put(item("api", testNow.Add(-24*time.Hour), 1<<20))
	f.listings.put(item("web", testNow.Add(-24*time.Hour), 2<<20))
	ws := f.firstCycle("platform")
	claims := f.claims()
	require.Len(t, claims, 1)
	assert.Equal(t, "9Gi", requestedString(&claims[0]), "ceil(4.5 Gi + 4 Gi)")
	assert.Equal(t, "9Gi", ws.Status.VolumeSize.String())
	assert.Equal(t, "4608Mi", ws.Status.NeededSize.String())
	assert.Nil(t, ws.Status.GrewFrom, "a claim made at its size did not grow")
	assert.Nil(t, ws.Status.StoredSize, "nothing measured before the first sync")

	// Measured: 1 GiB and 2 GiB, the need below the estimate: the share stays.
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	got := f.completeCycle(ws, testNow, entry(api, 1<<30), entry(web, 2<<30))
	assert.Equal(t, "9Gi", got.Status.VolumeSize.String(), "never shrunk")
	assert.Equal(t, "3Gi", got.Status.NeededSize.String())
	assert.Equal(t, "3Gi", got.Status.StoredSize.String(), "the measured mirrors")
	assert.Nil(t, got.Status.GrewFrom)

	// A push and a sync that measured 12 GiB: the need passes the headroom,
	// the share grows to 12 + 6 = 18 GiB after the sync.
	f.listings.put(item("web", f.clock().Add(time.Hour), 2<<20))
	f.advance(2 * time.Hour)
	f.requestSync("platform")
	f.reconcile("platform")
	ws = f.workspace("platform")
	require.NotNil(t, ws.Status.ActiveSync)
	assert.Equal(t, "9Gi", requestedString(&f.claims()[0]), "a push adds no estimate: nothing to grow before the fetch")
	got = f.completeCycle(ws, f.clock(), entry(api, 1<<30), entry(f.listings.items[testProvider+"/"+testOwner][1], 11<<30))
	assert.Equal(t, "18Gi", requestedString(&f.claims()[0]))
	assert.Equal(t, "18Gi", got.Status.VolumeSize.String())
	assert.Equal(t, "12Gi", got.Status.NeededSize.String())
	assert.Equal(t, "12Gi", got.Status.StoredSize.String())
	require.NotNil(t, got.Status.GrewFrom)
	assert.Equal(t, "9Gi", got.Status.GrewFrom.String(), "the sync grew the share from 9 GiB")

	// A repository added with 10 GiB reported: 15 GiB estimated on top of
	// the 12 measured; the share grows before the mirror is cloned.
	f.listings.put(item("sdk", f.clock(), 10<<20))
	f.updateSpec("platform", func(ws *v1alpha1.Workspace) { ws.Spec.Sources[0].Repositories = []string{"api", "web", "sdk"} })
	f.reconcile("platform")
	got = f.workspace("platform")
	require.NotNil(t, got.Status.ActiveSync)
	assert.Equal(t, "41Gi", requestedString(&f.claims()[0]), "12 + 15 need, 13.5 headroom, rounded up")
	assert.Equal(t, "41Gi", got.Status.VolumeSize.String())
	require.NotNil(t, got.Status.GrewFrom)
	assert.Equal(t, "18Gi", got.Status.GrewFrom.String(), "grown before the mirroring")

	// The sync measured 13 GiB: the 41 GiB share has room, and the size it
	// grew from before the mirroring stands for the cycle.
	items := f.listings.items[testProvider+"/"+testOwner]
	got = f.completeCycle(got, f.clock(), entry(items[0], 1<<30), entry(items[1], 11<<30), entry(items[2], 1<<30))
	assert.Equal(t, "41Gi", got.Status.VolumeSize.String())
	assert.Equal(t, "13Gi", got.Status.StoredSize.String())
	require.NotNil(t, got.Status.GrewFrom)
	assert.Equal(t, "18Gi", got.Status.GrewFrom.String())

	// The next cycle, with nothing changed, grows nothing: the marker goes.
	f.advance(2 * time.Hour)
	f.requestSync("platform")
	f.reconcile("platform")
	got = f.workspace("platform")
	assert.Equal(t, v1alpha1.SyncNoChange, got.Status.LastSync.Result)
	assert.Nil(t, got.Status.GrewFrom)
	assert.Equal(t, "13Gi", got.Status.StoredSize.String())
}

// TestTheCeilingFailsTheCycle: a size past the installation's ceiling fails
// the cycle before any clone, naming the size and the ceiling, and keeps
// the mirrors; after a sync the share grows to the ceiling at most.
func TestTheCeilingFailsTheCycle(t *testing.T) {
	f := newFixture(t, testProvisionedClass, workspaceNamed("platform"))
	ceiling := resource.MustParse("8Gi")
	f.c.sizing.MaxSize = &ceiling
	f.listings.put(item("api", testNow.Add(-24*time.Hour), 1<<20))
	f.listings.put(item("web", testNow.Add(-24*time.Hour), 2<<20))

	requeue := f.reconcile("platform")
	got := f.workspace("platform")
	assert.Equal(t, time.Hour, requeue)
	assert.Empty(t, f.claims(), "nothing claimed past the ceiling")
	assert.Empty(t, f.jobs())
	assert.Equal(t, v1alpha1.SyncFailed, got.Status.LastSync.Result)
	assert.Equal(t, "the volume needs 9Gi, past the installation's ceiling of 8Gi", got.Status.LastSync.Message)
	assert.Equal(t, v1alpha1.ReasonSizeCeiling, syncedCondition(t, got).Reason)

	// Within the ceiling the cycle runs; the measured need past it grows
	// the share to the ceiling and says so.
	ceiling = resource.MustParse("20Gi")
	f.c.sizing.MaxSize = &ceiling
	f.advance(time.Hour)
	ws := f.firstCycle("platform")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	got = f.completeCycle(ws, f.clock(), entry(api, 1<<30), entry(web, 15<<30))
	assert.Equal(t, v1alpha1.SyncSucceeded, got.Status.LastSync.Result)
	assert.Equal(t, "20Gi", requestedString(&f.claims()[0]))
	assert.Contains(t, got.Status.LastSync.Message, "past the installation's ceiling of 20Gi")
}

// TestADeletedWorkspaceIsLeftAlone: a Workspace being deleted gets no cycle.
func TestADeletedWorkspaceIsLeftAlone(t *testing.T) {
	ws := workspaceNamed("platform")
	ws.DeletionTimestamp = &metav1.Time{Time: testNow}
	ws.Finalizers = []string{"test/keep"}
	f := newFixture(t, testClass, ws)
	assert.Zero(t, f.reconcile("platform"))
	assert.Zero(t, f.listings.count())
	assert.Empty(t, f.jobs())
}

// TestStatusUpdateRetriesOnConflict: a status write that conflicts is made
// again from a fresh read.
func TestStatusUpdateRetriesOnConflict(t *testing.T) {
	f := newFixture(t, testClass, workspaceNamed("platform"))
	conflicts := 0
	f.dyn.PrependReactor("update", "workspaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(v1alpha1.WorkspaceResource.GroupResource(), "platform", nil)
		}
		return false, nil, nil
	})
	f.reconcile("platform")
	assert.Equal(t, 1, conflicts)
	require.NotNil(t, f.workspace("platform").Status.ActiveSync)
}

// TestRunSyncsEveryWorkspace: the loop runs the cycle of a Workspace that
// exists at start and of one created later, is woken by its Job's end, and
// stops with its context.
func TestRunSyncsEveryWorkspace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t, testClass, workspaceNamed("first"))
	done := make(chan error, 1)
	go func() { done <- f.c.Run(ctx) }()

	running := func(name string) func() bool {
		return func() bool {
			ws := f.workspace(name)
			return ws.Status.ActiveSync != nil
		}
	}
	require.Eventually(t, running("first"), 10*time.Second, 20*time.Millisecond)
	_, err := f.dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Create(ctx, toUnstructured(t, workspaceNamed("second")), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, running("second"), 10*time.Second, 20*time.Millisecond)

	// The Job's end wakes the loop: the outcome lands without a resync.
	ws := f.workspace("first")
	api, web := f.listings.items[testProvider+"/"+testOwner][0], f.listings.items[testProvider+"/"+testOwner][1]
	f.writeManifest(ws, testNow, entry(api, 2<<20), entry(web, 3<<20))
	f.finishJob(ws, batchv1.JobComplete, testNow)
	require.Eventually(t, func() bool {
		got := f.workspace("first")
		return got.Status.ActiveSync == nil && got.Status.LastSync != nil && got.Status.LastSync.Result == v1alpha1.SyncSucceeded
	}, 10*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}

// TestLeaderElectionRunsOneController: two replicas on one Lease run one
// controller: one Workspace gets one cycle, not two; the replica that led
// stops with ErrLeadershipLost when it loses the lease, and the other takes
// over.
func TestLeaderElectionRunsOneController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFixture(t, testClass, workspaceNamed("platform"))
	election := func(identity string) *LeaderElection {
		return &LeaderElection{Identity: identity, LeaseDuration: time.Second, RenewDeadline: 500 * time.Millisecond, RetryPeriod: 50 * time.Millisecond}
	}
	a := f.c
	a.election = election("replica-a")
	b := New(Config{Dynamic: f.dyn, Core: f.core, Namespace: testNamespace, StorageClass: testClass, Resync: time.Hour,
		Listings: f.listings, Instances: []provider.Instance{{Name: testProvider, KindName: "stub", Kind: stubKind{}}}, Now: f.clock,
		LeaderElection: election("replica-b")})
	// The third holder's conflicts, armed later: the reactor is in place
	// before any replica renews, since the fake's reactor chain is read
	// without a lock. A replica's update and the third holder's takeover
	// are serialised by mu: a renewal landing after the takeover would put
	// the replica back as the holder, and its release would then retry its
	// conflicts forever against a fake that ignores the release's deadline.
	var (
		mu    sync.Mutex
		taken bool
	)
	leases := coordinationv1.SchemeGroupVersion.WithResource("leases")
	update := k8stesting.ObjectReaction(f.core.Tracker())
	f.core.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if taken {
			return true, nil, apierrors.NewConflict(coordinationv1.Resource("leases"), DefaultLease, errors.New("another holder"))
		}
		return update(action)
	})
	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() { doneA <- a.Run(ctxA) }()
	require.Eventually(t, a.Leading, 10*time.Second, 20*time.Millisecond, "the first replica leads")
	go func() { doneB <- b.Run(ctx) }()

	require.Eventually(t, func() bool { return f.workspace("platform").Status.ActiveSync != nil }, 10*time.Second, 20*time.Millisecond)
	time.Sleep(500 * time.Millisecond)
	assert.False(t, b.Leading(), "the second replica waits")
	assert.Equal(t, 1, f.listings.count(), "one cycle, by the leader alone")
	assert.Len(t, f.jobs(), 1)

	// The leader goes: the lease is released and the other replica leads.
	cancelA()
	select {
	case err := <-doneA:
		require.NoError(t, err, "a stop is no lost lease")
	case <-time.After(10 * time.Second):
		t.Fatal("the leader did not stop")
	}
	require.Eventually(t, b.Leading, 10*time.Second, 20*time.Millisecond, "the second replica takes over")
	assert.Equal(t, 1, f.listings.count(), "the running Job is not restarted")

	// Another holder takes the lease from under the leader, whose renewals
	// the API server then refuses as conflicts: ErrLeadershipLost. (The
	// fake checks no resourceVersion, so the conflict is the reactor's.)
	holder := func() string {
		obj, err := f.core.Tracker().Get(leases, testNamespace, DefaultLease)
		require.NoError(t, err)
		return ptr.Deref(obj.(*coordinationv1.Lease).Spec.HolderIdentity, "")
	}
	mu.Lock()
	obj, err := f.core.Tracker().Get(leases, testNamespace, DefaultLease)
	require.NoError(t, err)
	lease := obj.(*coordinationv1.Lease)
	lease.Spec.HolderIdentity = ptr.To("replica-c")
	lease.Spec.AcquireTime = &metav1.MicroTime{Time: time.Now()}
	lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
	require.NoError(t, f.core.Tracker().Update(leases, lease, testNamespace))
	taken = true
	mu.Unlock()
	select {
	case err := <-doneB:
		require.ErrorIs(t, err, ErrLeadershipLost)
	case <-time.After(10 * time.Second):
		t.Fatalf("the replica did not notice the lost lease: the lease is held by %q, the replica leading %t", holder(), b.Leading())
	}
	assert.Equal(t, "replica-c", holder(), "the third holder keeps the lease")
	assert.False(t, b.Leading())
}

// TestSizing: the need counts measured mirrors, dropped ones included,
// the estimate for unmirrored repositories and the session directories;
// the headroom is the larger of the floor, the share and the Workspace's;
// the size at least the minimum, rounded up to the GiB; the ceiling
// refuses a size past it.
func TestSizing(t *testing.T) {
	q := resource.MustParse
	s := DefaultSizing
	gib := int64(1) << 30
	dropped := testNow
	manifest := &mirror.Manifest{Repositories: []mirror.Entry{
		{Owner: "acme", Name: "api", SizeBytes: 2 * gib},
		{Owner: "acme", Name: "old", SizeBytes: gib, DroppedAt: &dropped},
	}}
	items := []provider.Item{item("api", testNow, 10<<20), item("web", testNow, 2<<20)}
	sessions := []v1alpha1.SessionDirectory{{Session: "s1", Size: ptr.To(q("512Mi"))}, {Session: "s2"}}
	need := s.Need(manifest, items, sessions)
	assert.Equal(t, 2*gib+gib+3*gib+512<<20, need, "api measured, old on the volume still, web estimated at 1.5 × 2 GiB, s1 measured")
	assert.Equal(t, int64(3)*gib, s.Estimate(items[1]))
	assert.Zero(t, s.Need(nil, nil, nil))

	for name, tc := range map[string]struct {
		need int64
		spec *v1alpha1.Sizing
		want string
	}{
		"nothing":                 {0, nil, "4Gi"},
		"small need, the floor":   {gib, nil, "5Gi"},
		"rounded up":              {gib + 1, nil, "6Gi"},
		"large need, the share":   {20 * gib, nil, "30Gi"},
		"workspace headroom wins": {20 * gib, &v1alpha1.Sizing{Headroom: ptr.To(q("15Gi"))}, "35Gi"},
		"smaller ws headroom":     {20 * gib, &v1alpha1.Sizing{Headroom: ptr.To(q("1Gi"))}, "30Gi"},
		"minimum over computed":   {gib, &v1alpha1.Sizing{Minimum: ptr.To(q("32Gi"))}, "32Gi"},
		"minimum below computed":  {20 * gib, &v1alpha1.Sizing{Minimum: ptr.To(q("8Gi"))}, "30Gi"},
	} {
		t.Run(name, func(t *testing.T) {
			got := s.Size(tc.need, tc.spec)
			assert.Equal(t, tc.want, got.String())
		})
	}

	capped := Sizing{Factor: 1.5, HeadroomFloor: q("4Gi"), HeadroomShare: 0.5, MaxSize: ptr.To(q("100Gi"))}
	assert.NoError(t, capped.Exceeds(q("100Gi")))
	assert.EqualError(t, capped.Exceeds(q("101Gi")), "the volume needs 101Gi, past the installation's ceiling of 100Gi")
	assert.NoError(t, s.Exceeds(q("1Ti")), "no ceiling")
}

func TestSizingValidate(t *testing.T) {
	q := resource.MustParse
	require.NoError(t, DefaultSizing.Validate())
	assert.Error(t, Sizing{Factor: 0.9, HeadroomFloor: q("1Gi")}.Validate())
	assert.Error(t, Sizing{Factor: 1, HeadroomFloor: q("-1Gi")}.Validate())
	assert.Error(t, Sizing{Factor: 1, HeadroomShare: -0.1}.Validate())
	assert.Error(t, Sizing{Factor: 1, MaxSize: ptr.To(q("0"))}.Validate())
}

// TestNewDefaults: an empty sizing, cleanup window and resync take the
// defaults.
func TestNewDefaults(t *testing.T) {
	f := newFixture(t, testClass)
	c := New(Config{Dynamic: f.dyn, Core: f.core, Namespace: testNamespace})
	assert.Equal(t, DefaultSizing, c.sizing)
	assert.Equal(t, DefaultSessionCleanupAfter, c.sessionCleanupAfter)
	assert.Equal(t, DefaultResync, c.resync)
	assert.NotNil(t, c.jobs.Template, "the default Job template")
}

// requestedString is the claim's storage request as a string.
func requestedString(pvc *corev1.PersistentVolumeClaim) string {
	q := Requested(pvc)
	return q.String()
}
