//go:build envtest

package workspace

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

// TestCRDValidation installs the CRD the chart ships into a real API server
// and proves its CEL and schema rules refuse what the schema can check.
func TestCRDValidation(t *testing.T) {
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
	_, err = core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	client := dyn.Resource(v1alpha1.WorkspaceResource).Namespace(testNamespace)

	create := func(t *testing.T, doc string) (*unstructured.Unstructured, error) {
		t.Helper()
		u := &unstructured.Unstructured{}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &u.Object))
		return client.Create(ctx, u, metav1.CreateOptions{})
	}

	refused := []struct{ name, doc, message string }{
		{"empty source", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: empty-source}
spec:
  organization: acme
  sources:
  - {provider: github, owner: acme}`, "a source needs at least one selector"},
		{"empty selectors", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: empty-selectors}
spec:
  organization: acme
  sources:
  - {provider: github, owner: acme, repositories: [], filters: {languages: []}, exclude: [api]}`, "a source needs at least one selector"},
		{"no source", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: no-source}
spec: {organization: acme, sources: []}`, "spec.sources: Invalid value"},
		{"invalid weekday", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: bad-weekday}
spec:
  organization: acme
  sources: [{provider: github, owner: acme, repositories: [api]}]
  sync: {schedule: custom, weekdays: [monday, funday], hour: 3}`, `spec.sync.weekdays[1]: Unsupported value: "funday"`},
		{"hour above 23", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: bad-hour}
spec:
  organization: acme
  sources: [{provider: github, owner: acme, repositories: [api]}]
  sync: {schedule: custom, weekdays: [monday], hour: 24}`, "spec.sync.hour: Invalid value: 24"},
		{"negative hour", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: negative-hour}
spec:
  organization: acme
  sources: [{provider: github, owner: acme, repositories: [api]}]
  sync: {schedule: custom, weekdays: [monday], hour: -1}`, "spec.sync.hour: Invalid value: -1"},
		{"custom without weekdays", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: custom-no-days}
spec:
  organization: acme
  sources: [{provider: github, owner: acme, repositories: [api]}]
  sync: {schedule: custom, hour: 3}`, "weekdays and hour are set for, and only for, a custom schedule"},
		{"weekdays on nightly", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: nightly-days}
spec:
  organization: acme
  sources: [{provider: github, owner: acme, repositories: [api]}]
  sync: {schedule: nightly, weekdays: [monday]}`, "weekdays and hour are set for, and only for, a custom schedule"},
		{"negative headroom", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: negative-headroom}
spec:
  organization: acme
  sources: [{provider: github, owner: acme, repositories: [api]}]
  sizing: {headroom: -4Gi}`, "headroom must not be negative"},
		{"negative minimum", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: negative-minimum}
spec:
  organization: acme
  sources: [{provider: github, owner: acme, repositories: [api]}]
  sizing: {minimum: -1}`, "minimum must not be negative"},
		{"invalid topic match", `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: bad-match}
spec:
  organization: acme
  sources: [{provider: github, owner: acme, filters: {topics: {match: some, names: [agent]}}}]`, `Unsupported value: "some"`},
	}
	for _, c := range refused {
		t.Run("refuses "+c.name, func(t *testing.T) {
			_, err := create(t, c.doc)
			require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
			require.ErrorContains(t, err, c.message)
			t.Log(err)
		})
	}

	t.Run("accepts and defaults", func(t *testing.T) {
		u, err := create(t, `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: platform}
spec:
  organization: acme
  sources:
  - {provider: github, owner: acme, repositories: [api, web]}
  - {provider: github, owner: acme-labs, filters: {languages: [Go], topics: {names: [agent]}}, exclude: [legacy]}
  sizing: {headroom: 8Gi, minimum: "0"}`)
		require.NoError(t, err)
		ws, err := fromUnstructured(u)
		require.NoError(t, err)
		require.Equal(t, v1alpha1.ScheduleNightly, ws.Spec.Sync.Schedule)
		require.Equal(t, v1alpha1.TopicMatchAny, ws.Spec.Sources[1].Filters.Topics.Match)
		require.False(t, ws.Spec.Sources[1].IncludeArchived)
		require.False(t, ws.Spec.Sources[1].IncludeForks)

		_, err = create(t, `
apiVersion: workspace-manager.giantswarm.io/v1alpha1
kind: Workspace
metadata: {name: weekly-custom}
spec:
  organization: acme
  sources: [{provider: github, owner: acme, filters: {topics: {match: all, names: [agent, go]}}}]
  sync: {schedule: custom, weekdays: [monday, thursday], hour: 0}`)
		require.NoError(t, err)
	})

	t.Run("organization is immutable", func(t *testing.T) {
		u, err := client.Get(ctx, "platform", metav1.GetOptions{})
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedField(u.Object, "umbrella", "spec", "organization"))
		_, err = client.Update(ctx, u, metav1.UpdateOptions{})
		require.True(t, apierrors.IsInvalid(err), "want Invalid, got %v", err)
		require.ErrorContains(t, err, "organization is immutable")
	})

	t.Run("store against the API server", func(t *testing.T) {
		s := NewStore(dyn, testNamespace, testOrgs, []string{"github"})
		list, err := s.List(caller("acme-devs"))
		require.NoError(t, err)
		require.Len(t, list, 2)
		list, err = s.List(caller("umbrella-devs"))
		require.NoError(t, err)
		require.Empty(t, list)
		require.ErrorIs(t, s.Delete(caller("umbrella-devs"), "platform"), ErrForbidden)
		require.NoError(t, s.Delete(caller("acme-devs"), "platform"))
	})
}
