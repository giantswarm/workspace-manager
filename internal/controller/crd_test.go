package controller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"
)

// shippedCRD is the Workspace CRD the chart ships.
func shippedCRD(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "helm", "workspace-manager", "files", "crds", "workspace-manager.giantswarm.io_workspaces.yaml"))
	require.NoError(t, err)
	obj := map[string]any{}
	require.NoError(t, yaml.Unmarshal(b, &obj))
	return &unstructured.Unstructured{Object: obj}
}

// statusSchema is the shipped CRD's status schema of its first version.
func statusSchema(t *testing.T, crd *unstructured.Unstructured) map[string]any {
	t.Helper()
	status, found := nestedMap(firstVersion(t, crd), "schema", "openAPIV3Schema", "properties", "status")
	require.True(t, found)
	return status
}

// firstVersion is the shipped CRD's first version, to change in place.
func firstVersion(t *testing.T, crd *unstructured.Unstructured) map[string]any {
	t.Helper()
	versions, _, _ := unstructured.NestedFieldNoCopy(crd.Object, "spec", "versions")
	require.NotEmpty(t, versions)
	return versions.([]any)[0].(map[string]any)
}

func checkCRD(t *testing.T, objs ...runtime.Object) error {
	t.Helper()
	return CheckCRD(t.Context(), dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objs...))
}

func TestCheckCRDAcceptsTheShippedCRD(t *testing.T) {
	require.NoError(t, checkCRD(t, shippedCRD(t)))
}

func TestCheckCRDRefusesACRDWithoutActiveSync(t *testing.T) {
	crd := shippedCRD(t)
	delete(statusSchema(t, crd)["properties"].(map[string]any), "activeSync")

	err := checkCRD(t, crd)
	require.Error(t, err)
	require.ErrorContains(t, err, "workspaces.workspace-manager.giantswarm.io")
	require.ErrorContains(t, err, "status.activeSync")
}

func TestCheckCRDAcceptsAStatusThatKeepsUnknownFields(t *testing.T) {
	crd := shippedCRD(t)
	status := statusSchema(t, crd)
	delete(status["properties"].(map[string]any), "activeSync")
	status["x-kubernetes-preserve-unknown-fields"] = true

	require.NoError(t, checkCRD(t, crd))
}

func TestCheckCRDRefusesAMissingCRD(t *testing.T) {
	err := checkCRD(t)
	require.ErrorContains(t, err, "workspaces.workspace-manager.giantswarm.io is not installed")
}

func TestCheckCRDRefusesACRDThatDoesNotServeTheVersion(t *testing.T) {
	crd := shippedCRD(t)
	firstVersion(t, crd)["served"] = false

	require.ErrorContains(t, checkCRD(t, crd), "does not serve v1alpha1")
}
