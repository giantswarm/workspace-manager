//go:build envtest

package controller

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// An API server serving the shipped CRD passes the check; the same CRD
// updated to drop status.activeSync, the skew the check guards against, is
// refused by name.
func TestCheckCRDAgainstTheAPIServer(t *testing.T) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "helm", "workspace-manager", "files", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err, "envtest needs KUBEBUILDER_ASSETS (make test-envtest sets it)")
	t.Cleanup(func() { _ = env.Stop() })
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	ctx := t.Context()

	require.NoError(t, CheckCRD(ctx, dyn))

	crd, err := dyn.Resource(CRDResource).Get(ctx, WorkspaceCRD, metav1.GetOptions{})
	require.NoError(t, err)
	status := statusSchema(t, crd)
	delete(status["properties"].(map[string]any), "activeSync")
	_, err = dyn.Resource(CRDResource).Update(ctx, crd, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.ErrorContains(t, CheckCRD(ctx, dyn), "the Workspace CRD workspaces.workspace-manager.giantswarm.io (version v1alpha1) has no status.activeSync")
}
