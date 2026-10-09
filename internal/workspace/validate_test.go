package workspace

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

func workspace(name, org string, providers ...string) *v1alpha1.Workspace {
	ws := &v1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       v1alpha1.WorkspaceSpec{Organization: org, Sync: v1alpha1.Sync{Schedule: v1alpha1.ScheduleNightly}},
	}
	for _, p := range providers {
		ws.Spec.Sources = append(ws.Spec.Sources, v1alpha1.Source{Provider: p, Owner: "acme", Repositories: []string{"api"}})
	}
	return ws
}

func TestValidateUnknownProvider(t *testing.T) {
	require.NoError(t, Validate(workspace("w", "acme", "github", "ghes"), []string{"ghes", "github"}))

	err := Validate(workspace("w", "acme", "github", "gitlab"), []string{"github", "ghes"})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, `sources[1]: unknown provider instance "gitlab" (configured: ghes, github)`)
	require.NotContains(t, err.Error(), "sources[0]")

	err = Validate(workspace("w", "acme", "github"), nil)
	require.ErrorContains(t, err, `unknown provider instance "github" (configured: none)`)
}
