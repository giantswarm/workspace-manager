package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

// Status writes a Workspace's status, the controller's alone, through the
// status subresource (the store never writes it).
type Status struct {
	client dynamic.ResourceInterface
}

// NewStatus returns the status writer of the Workspaces of namespace.
func NewStatus(client dynamic.Interface, namespace string) *Status {
	return &Status{client: client.Resource(v1alpha1.WorkspaceResource).Namespace(namespace)}
}

// Update reads the Workspace, applies mutate to it and writes its status when
// mutate reports a change; a conflict is retried from a fresh read.
func (s *Status) Update(ctx context.Context, name string, mutate func(*v1alpha1.Workspace) bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		u, err := s.client.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get workspace %q: %w", name, err)
		}
		ws, err := fromUnstructured(u)
		if err != nil {
			return err
		}
		if !mutate(ws) {
			return nil
		}
		obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ws)
		if err != nil {
			return fmt.Errorf("workspace %q: %w", name, err)
		}
		if _, err := s.client.UpdateStatus(ctx, &unstructured.Unstructured{Object: obj}, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				return err
			}
			return fmt.Errorf("update status of workspace %q: %w", name, err)
		}
		return nil
	})
}

func fromUnstructured(u *unstructured.Unstructured) (*v1alpha1.Workspace, error) {
	ws := &v1alpha1.Workspace{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ws); err != nil {
		return nil, fmt.Errorf("workspace %q: %w", u.GetName(), err)
	}
	return ws, nil
}
