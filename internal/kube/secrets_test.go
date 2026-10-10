package kube

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/internal/provider"
)

func TestSecretsValue(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-oauth", Namespace: "wm"},
		Data:       map[string][]byte{"client-secret": []byte("s3cr3t")},
	})
	s := Secrets{Client: client, Namespace: "wm"}

	v, err := s.Value(context.Background(), provider.SecretRef{Name: "github-oauth", Key: "client-secret"})
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t", string(v))

	_, err = s.Value(context.Background(), provider.SecretRef{Name: "github-oauth", Key: "other"})
	assert.ErrorContains(t, err, `has no key "other"`)
	assert.NotContains(t, err.Error(), "s3cr3t")

	_, err = s.Value(context.Background(), provider.SecretRef{Name: "missing", Key: "k"})
	assert.ErrorContains(t, err, "wm/missing not found")
}
