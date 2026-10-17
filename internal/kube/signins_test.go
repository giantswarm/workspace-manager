package kube

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestPurgeSignIns: the stored sign-ins and their refresh Leases go in one
// collection delete each, selected by their labels (the Secrets by their type
// too), in the manager's namespace only, and no Secret is read.
func TestPurgeSignIns(t *testing.T) {
	client := fake.NewClientset()
	type deleted struct{ resource, namespace, labels, fields string }
	var got []deleted
	client.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		switch a := action.(type) {
		case k8stesting.DeleteCollectionAction:
			r := a.GetListRestrictions()
			got = append(got, deleted{a.GetResource().Resource, a.GetNamespace(), r.Labels.String(), r.Fields.String()})
			return true, nil, nil
		default:
			t.Errorf("unexpected %s of %s", action.GetVerb(), action.GetResource().Resource)
			return true, nil, nil
		}
	})

	require.NoError(t, PurgeSignIns(context.Background(), client, "wm"))
	labels := "app.kubernetes.io/managed-by=workspace-manager,workspace-manager.giantswarm.io/person,workspace-manager.giantswarm.io/provider-instance"
	assert.Equal(t, []deleted{
		{"secrets", "wm", labels, "type=" + SignInSecretType},
		{"leases", "wm", labels, ""},
	}, got)
}
