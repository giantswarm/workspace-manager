package kube

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWriteConfigMapCreatesAndReplacesData(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()

	require.NoError(t, WriteConfigMap(ctx, cs, "kagent", "sync-1", map[string]string{"manifest.json": "{}"}))
	cm, err := cs.CoreV1().ConfigMaps("kagent").Get(ctx, "sync-1", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"manifest.json": "{}"}, cm.Data)

	// A ConfigMap the controller created ahead keeps its metadata.
	cm.Labels = map[string]string{"workspace": "w1"}
	cm.Data = map[string]string{"stale": "x"}
	_, err = cs.CoreV1().ConfigMaps("kagent").Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, WriteConfigMap(ctx, cs, "kagent", "sync-1", map[string]string{"manifest.json": `{"a":1}`}))
	cm, err = cs.CoreV1().ConfigMaps("kagent").Get(ctx, "sync-1", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"workspace": "w1"}, cm.Labels)
	require.Equal(t, map[string]string{"manifest.json": `{"a":1}`}, cm.Data)
	require.IsType(t, &corev1.ConfigMap{}, cm)
}
