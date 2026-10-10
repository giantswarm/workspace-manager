package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/workspace-manager/internal/provider"
)

// Secrets reads the values provider instances reference from Secrets in one
// namespace, the manager's. Errors name the Secret and key, never a value.
type Secrets struct {
	Client    kubernetes.Interface
	Namespace string
}

var _ provider.Secrets = Secrets{}

// Value implements provider.Secrets.
func (s Secrets) Value(ctx context.Context, ref provider.SecretRef) ([]byte, error) {
	secret, err := s.Client.CoreV1().Secrets(s.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("secret %s/%s not found", s.Namespace, ref.Name)
	}
	if err != nil {
		return nil, fmt.Errorf("read secret %s/%s: %w", s.Namespace, ref.Name, err)
	}
	v, ok := secret.Data[ref.Key]
	if !ok || len(v) == 0 {
		return nil, fmt.Errorf("secret %s/%s has no key %q", s.Namespace, ref.Name, ref.Key)
	}
	return v, nil
}
