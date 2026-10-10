package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

const (
	testNamespace = "kagent"
	testClass     = "efs-rwx"
)

// workspace is a Workspace as the API server serves it: typed, with a UID.
func workspace(name string) *v1alpha1.Workspace {
	return &v1alpha1.Workspace{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Workspace"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: types.UID("uid-" + name), Generation: 3},
		Spec: v1alpha1.WorkspaceSpec{
			Organization: "acme",
			Sources:      []v1alpha1.Source{{Provider: "github", Owner: "acme", Repositories: []string{"api"}}},
		},
	}
}

func toUnstructured(t *testing.T, ws *v1alpha1.Workspace) *unstructured.Unstructured {
	t.Helper()
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ws)
	require.NoError(t, err)
	return &unstructured.Unstructured{Object: obj}
}

type clients struct {
	dynamic dynamic.Interface
	core    kubernetes.Interface
}

// newTestController builds a controller on fake clients holding the given
// Workspaces; class is its StorageClass.
func newTestController(t *testing.T, class string, wss ...*v1alpha1.Workspace) (*Controller, clients) {
	t.Helper()
	var initial []runtime.Object
	for _, ws := range wss {
		initial = append(initial, toUnstructured(t, ws))
	}
	// An empty scheme: the fake keeps the objects unstructured, as the API
	// server serves them to the dynamic client.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{v1alpha1.WorkspaceResource: "WorkspaceList"}, initial...)
	core := fake.NewClientset()
	c := New(Config{Dynamic: dyn, Core: core, Namespace: testNamespace, StorageClass: class, Resync: time.Hour})
	return c, clients{dynamic: dyn, core: core}
}

func (c clients) workspace(t *testing.T, name string) *v1alpha1.Workspace {
	t.Helper()
	u, err := c.dynamic.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	ws, err := fromUnstructured(u)
	require.NoError(t, err)
	return ws
}

func (c clients) claims(t *testing.T) []corev1.PersistentVolumeClaim {
	t.Helper()
	list, err := c.core.CoreV1().PersistentVolumeClaims(testNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	return list.Items
}

// TestReconcileClaimsFromTheStorageClass: a Workspace gets one ReadWriteMany
// claim naming the StorageClass, owned by the Workspace, at the nominal size;
// its status names the claim and the condition is True. A second reconcile
// changes nothing.
func TestReconcileClaimsFromTheStorageClass(t *testing.T) {
	ctx := context.Background()
	ws := workspace("platform")
	c, k := newTestController(t, testClass, ws)

	require.NoError(t, c.Reconcile(ctx, ws))

	claims := k.claims(t)
	require.Len(t, claims, 1)
	pvc := claims[0]
	assert.Equal(t, "workspace-platform", pvc.Name)
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, pvc.Spec.AccessModes)
	assert.Equal(t, testClass, ptr.Deref(pvc.Spec.StorageClassName, ""))
	assert.True(t, NominalSize.Equal(pvc.Spec.Resources.Requests[corev1.ResourceStorage]), "nominal size, nothing measured yet")
	assert.Equal(t, map[string]string{LabelWorkspace: "platform"}, pvc.Labels)
	require.Len(t, pvc.OwnerReferences, 1)
	owner := pvc.OwnerReferences[0]
	assert.Equal(t, "Workspace", owner.Kind)
	assert.Equal(t, v1alpha1.GroupVersion.String(), owner.APIVersion)
	assert.Equal(t, ws.UID, owner.UID)
	assert.True(t, ptr.Deref(owner.Controller, false), "the Workspace controls the claim")
	assert.True(t, ptr.Deref(owner.BlockOwnerDeletion, false))

	got := k.workspace(t, "platform")
	require.NotNil(t, got.Status.Volume)
	assert.Equal(t, "workspace-platform", got.Status.Volume.ClaimName)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionVolumeClaimed)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, v1alpha1.ReasonVolumeClaimed, cond.Reason)
	assert.Equal(t, ws.Generation, cond.ObservedGeneration)
	assert.Contains(t, cond.Message, "workspace-platform")
	assert.Contains(t, cond.Message, testClass)
	assert.Equal(t, "acme", got.Spec.Organization, "the spec is untouched")

	// Idempotent: the same claim, no second create, no status write.
	writes := 0
	k.dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("update", "workspaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		writes++
		return false, nil, nil
	})
	require.NoError(t, c.Reconcile(ctx, got))
	assert.Len(t, k.claims(t), 1)
	assert.Zero(t, writes, "an unchanged status is not written")
}

// TestReconcileFollowsTheSizing: the claim requests the spec's minimum, and
// the provisioned size the status reports over it.
func TestReconcileFollowsTheSizing(t *testing.T) {
	ctx := context.Background()
	minimum := resource.MustParse("32Gi")
	ws := workspace("sized")
	ws.Spec.Sizing = &v1alpha1.Sizing{Minimum: &minimum}
	c, k := newTestController(t, testClass, ws)
	require.NoError(t, c.Reconcile(ctx, ws))
	claims := k.claims(t)
	require.Len(t, claims, 1)
	assert.True(t, minimum.Equal(claims[0].Spec.Resources.Requests[corev1.ResourceStorage]), "the spec's minimum")

	provisioned := resource.MustParse("48Gi")
	measured := workspace("measured")
	measured.Spec.Sizing = &v1alpha1.Sizing{Minimum: &minimum}
	measured.Status.VolumeSize = &provisioned
	c, k = newTestController(t, testClass, measured)
	require.NoError(t, c.Reconcile(ctx, measured))
	claims = k.claims(t)
	require.Len(t, claims, 1)
	assert.True(t, provisioned.Equal(claims[0].Spec.Resources.Requests[corev1.ResourceStorage]), "the status's provisioned size wins")
}

// TestReconcileRefusesWithoutAStorageClass: without a class no claim is made,
// the condition names the missing flag and chart value, and the cluster's
// default class is not used in its place.
func TestReconcileRefusesWithoutAStorageClass(t *testing.T) {
	ctx := context.Background()
	ws := workspace("platform")
	c, k := newTestController(t, "", ws)

	err := c.Reconcile(ctx, ws)
	require.ErrorIs(t, err, ErrNoStorageClass)
	assert.Empty(t, k.claims(t), "no claim, not even from the default class")

	got := k.workspace(t, "platform")
	assert.Nil(t, got.Status.Volume)
	cond := meta.FindStatusCondition(got.Status.Conditions, v1alpha1.ConditionVolumeClaimed)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v1alpha1.ReasonStorageClassMissing, cond.Reason)
	assert.Contains(t, cond.Message, "--storage-class")
	assert.Contains(t, cond.Message, "storage.storageClassName")
	assert.Equal(t, ws.Generation, cond.ObservedGeneration)
}

// TestReconcileRecordsARefusedClaim: an API server error on the claim lands
// in the condition and is returned, so the loop retries.
func TestReconcileRecordsARefusedClaim(t *testing.T) {
	ctx := context.Background()
	ws := workspace("platform")
	c, k := newTestController(t, testClass, ws)
	k.core.(*fake.Clientset).PrependReactor("create", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("persistentvolumeclaims"), "workspace-platform", nil)
	})

	err := c.Reconcile(ctx, ws)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoStorageClass)
	cond := meta.FindStatusCondition(k.workspace(t, "platform").Status.Conditions, v1alpha1.ConditionVolumeClaimed)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, v1alpha1.ReasonVolumeClaimFailed, cond.Reason)
	assert.Contains(t, cond.Message, "forbidden")
}

// TestStatusUpdateRetriesOnConflict: a status write that conflicts is made
// again from a fresh read.
func TestStatusUpdateRetriesOnConflict(t *testing.T) {
	ctx := context.Background()
	ws := workspace("platform")
	c, k := newTestController(t, testClass, ws)
	conflicts := 0
	k.dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor("update", "workspaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(v1alpha1.WorkspaceResource.GroupResource(), "platform", nil)
		}
		return false, nil, nil
	})
	require.NoError(t, c.Reconcile(ctx, ws))
	assert.Equal(t, 1, conflicts)
	require.NotNil(t, k.workspace(t, "platform").Status.Volume)
}

// TestRunClaimsEveryWorkspace: the loop claims the volume of a Workspace that
// exists at start and of one created later, and stops with its context.
func TestRunClaimsEveryWorkspace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, k := newTestController(t, testClass, workspace("first"))
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	claimed := func(name string) func() bool {
		return func() bool {
			_, err := k.core.CoreV1().PersistentVolumeClaims(testNamespace).Get(ctx, "workspace-"+name, metav1.GetOptions{})
			if err != nil {
				return false
			}
			cond := meta.FindStatusCondition(k.workspace(t, name).Status.Conditions, v1alpha1.ConditionVolumeClaimed)
			return cond != nil && cond.Status == metav1.ConditionTrue
		}
	}
	require.Eventually(t, claimed("first"), 10*time.Second, 20*time.Millisecond)

	_, err := k.dynamic.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace).Create(ctx, toUnstructured(t, workspace("second")), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, claimed("second"), 10*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
}
