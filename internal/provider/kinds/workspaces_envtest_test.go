//go:build envtest

package kinds

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/api"
)

// TestWorkspaceToolsEnvtest runs the tools against a real API server serving
// the chart's Workspace CRD: its schema and CEL rules refuse what the
// manager's own validation leaves to them.
func TestWorkspaceToolsEnvtest(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "helm", "workspace-manager", "files", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "envtest needs KUBEBUILDER_ASSETS (make test-envtest sets it)")
	t.Cleanup(func() { _ = env.Stop() })
	core, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	_, err = core.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: wsNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	f := newToolsFixture(t, kubeClients{dyn: dyn, core: core})

	workspaceToolsFlow(t, f)

	t.Run("the CRD's rules are the tool's answer, and nothing is written", func(t *testing.T) {
		for _, c := range []struct {
			args map[string]any
			want string
		}{
			{map[string]any{"name": "no-selector", "organization": "acme", "sources": []any{map[string]any{"provider": "github", "owner": "acme"}}},
				"a source needs at least one selector"},
			{map[string]any{"name": "Not_A_Name", "organization": "acme", "sources": platformSources}, "metadata.name"},
		} {
			refused(t, f.call(t, f.member, "create_workspace", c.args), "invalid workspace: ")
			refused(t, f.call(t, f.member, "create_workspace", c.args), c.want)
		}
		list, err := dyn.Resource(v1alpha1.WorkspaceResource).Namespace(wsNamespace).List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Empty(t, list.Items)
	})

	t.Run("a stale resourceVersion is refused", func(t *testing.T) {
		created := ok[api.Workspace](t, f.call(t, f.member, "create_workspace", map[string]any{"name": "stale", "organization": "acme", "sources": platformSources}))
		ok[api.Workspace](t, f.call(t, f.member, "update_workspace", map[string]any{"name": "stale", "sources": platformSources[1:]}))
		refused(t, f.call(t, f.member, "update_workspace", map[string]any{"name": "stale", "sources": platformSources, "resourceVersion": created.ResourceVersion}),
			"changed since it was read")
	})
}
