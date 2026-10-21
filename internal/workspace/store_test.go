package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

const testNamespace = "kagent"

var testOrgs = Organizations{"acme": {Groups: []string{"acme-devs"}}, "umbrella": {Groups: []string{"umbrella-devs"}}}

func newTestStore(t *testing.T, objs ...*v1alpha1.Workspace) *Store {
	t.Helper()
	// An empty scheme: the fake keeps the objects unstructured, as the API
	// server serves them to the dynamic client.
	scheme := runtime.NewScheme()
	var initial []runtime.Object
	for _, ws := range objs {
		u, err := toUnstructured(ws)
		require.NoError(t, err)
		initial = append(initial, u)
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{v1alpha1.WorkspaceResource: "WorkspaceList"}, initial...)
	return NewStore(client, testNamespace, testOrgs, []string{"github"})
}

// TestMembersOnly: a caller without the Organization's member group can
// neither read nor write its Workspaces; a member can do both.
func TestMembersOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, workspace("acme-ws", "acme", "github"), workspace("umbrella-ws", "umbrella", "github"))
	member, outsider := caller("acme-devs"), caller("umbrella-devs")

	// Read.
	list, err := s.List(member)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "acme-ws", list[0].Name)
	_, err = s.Get(member, "acme-ws")
	require.NoError(t, err)
	_, err = s.Get(outsider, "acme-ws")
	require.ErrorIs(t, err, ErrForbidden)
	list, err = s.List(ctx)
	require.NoError(t, err)
	require.Empty(t, list, "no caller sees nothing")

	// Create.
	_, err = s.Create(outsider, workspace("new", "acme", "github"))
	require.ErrorIs(t, err, ErrForbidden)
	_, err = s.Create(member, workspace("new", "acme", "github"))
	require.NoError(t, err)

	// Update.
	changed := workspace("acme-ws", "acme", "github")
	changed.Spec.Sources[0].Owner = "acme-labs"
	_, err = s.Update(outsider, changed)
	require.ErrorIs(t, err, ErrForbidden)
	got, err := s.Update(member, changed)
	require.NoError(t, err)
	require.Equal(t, "acme-labs", got.Spec.Sources[0].Owner)
	moved := workspace("acme-ws", "umbrella", "github")
	_, err = s.Update(member, moved)
	require.ErrorIs(t, err, ErrForbidden, "a member of acme cannot hand a workspace to umbrella")

	// Delete.
	_, err = s.Delete(outsider, "acme-ws", time.Now())
	require.ErrorIs(t, err, ErrForbidden)
	_, err = s.Get(member, "acme-ws")
	require.NoError(t, err, "still there")
	_, err = s.Delete(member, "acme-ws", time.Now())
	require.NoError(t, err)
	_, err = s.Get(member, "acme-ws")
	require.True(t, apierrors.IsNotFound(err), "%v", err)
}

// TestCreateRefusesUnknownProvider: a Workspace naming an unknown provider
// instance is refused, naming it, before anything is written.
func TestCreateRefusesUnknownProvider(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Create(caller("acme-devs"), workspace("w", "acme", "gitlab"))
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, `unknown provider instance "gitlab"`)
	list, err := s.List(caller("acme-devs"))
	require.NoError(t, err)
	require.Empty(t, list)

	ws := workspace("w", "acme", "github")
	_, err = s.Create(caller("acme-devs"), ws)
	require.NoError(t, err)
	ws.Spec.Sources[0].Provider = "gitlab"
	_, err = s.Update(caller("acme-devs"), ws)
	require.ErrorContains(t, err, `unknown provider instance "gitlab"`)
}

// TestADeletingWorkspaceIsAskedForItsCycle: on a Workspace being deleted, a
// sync request and a second delete record the request the controller runs
// the deletion cycle for; a non-member's records nothing.
func TestADeletingWorkspaceIsAskedForItsCycle(t *testing.T) {
	ws := workspace("acme-ws", "acme", "github")
	ws.DeletionTimestamp = &metav1.Time{Time: time.Date(2026, 10, 14, 11, 0, 0, 0, time.UTC)}
	ws.Finalizers = []string{v1alpha1.FinalizerVolume}
	s := newTestStore(t, ws)
	member, outsider := caller("acme-devs"), caller("umbrella-devs")
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	_, err := s.RequestSync(outsider, "acme-ws", at)
	require.ErrorIs(t, err, ErrForbidden)
	_, err = s.Delete(outsider, "acme-ws", at)
	require.ErrorIs(t, err, ErrForbidden)
	got, err := s.Get(member, "acme-ws")
	require.NoError(t, err)
	require.Empty(t, got.Annotations[v1alpha1.AnnotationSyncRequested], "a refusal records nothing")

	got, err = s.RequestSync(member, "acme-ws", at)
	require.NoError(t, err)
	require.Equal(t, "2026-10-14T12:00:00Z", got.Annotations[v1alpha1.AnnotationSyncRequested])
	require.Equal(t, "jane@example.com", got.Annotations[v1alpha1.AnnotationSyncRequestedBy])
	require.Equal(t, []string{"20261014T120000.000000000Z"}, v1alpha1.SyncRequestIDs(got))
	got, err = s.RequestSync(member, "acme-ws", at)
	require.NoError(t, err)
	require.Equal(t, []string{"20261014T120000.000000000Z", "20261014T120000.000000001Z"}, v1alpha1.SyncRequestIDs(got),
		"a second request at the same time follows the first")

	got, err = s.Delete(member, "acme-ws", at.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, got.DeletionTimestamp, "returned as the request left it")
	require.Equal(t, "2026-10-14T12:01:00Z", got.Annotations[v1alpha1.AnnotationSyncRequested])
	_, err = s.Get(member, "acme-ws")
	require.NoError(t, err, "still there: the controller lets it go")
}
