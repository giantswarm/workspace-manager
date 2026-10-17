package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The marks of the provider sign-ins earlier versions kept, a sealed Secret
// and a refresh Lease per person and provider instance: the person's token is
// muster's grant now, and none stays in the cluster.
const (
	// SignInSecretType is the type of the Secrets that held sealed sign-ins.
	SignInSecretType = "workspace-manager.giantswarm.io/signin"
	// SignInSelector selects the sign-in Secrets and their refresh Leases.
	SignInSelector = "app.kubernetes.io/managed-by=workspace-manager," +
		"workspace-manager.giantswarm.io/provider-instance," +
		"workspace-manager.giantswarm.io/person"
)

// PurgeSignIns deletes, in namespace, every sign-in Secret and refresh Lease
// an earlier version kept: one collection delete each, by their labels (and
// the Secrets by their type), reading none of them.
func PurgeSignIns(ctx context.Context, client kubernetes.Interface, namespace string) error {
	secrets := metav1.ListOptions{LabelSelector: SignInSelector, FieldSelector: "type=" + SignInSecretType}
	if err := client.CoreV1().Secrets(namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, secrets); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the stored provider sign-ins in %s: %w", namespace, err)
	}
	leases := metav1.ListOptions{LabelSelector: SignInSelector}
	if err := client.CoordinationV1().Leases(namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, leases); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete the sign-ins' refresh leases in %s: %w", namespace, err)
	}
	return nil
}
