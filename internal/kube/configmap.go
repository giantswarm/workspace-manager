package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// WriteConfigMap creates the ConfigMap, or replaces the data of an existing
// one and keeps its metadata: the sync Job's result, which the controller may
// have created ahead with its labels and owner.
func WriteConfigMap(ctx context.Context, cs kubernetes.Interface, namespace, name string, data map[string]string) error {
	cms := cs.CoreV1().ConfigMaps(namespace)
	existing, err := cms.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Data: data}
		if _, err := cms.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create configmap %s/%s: %w", namespace, name, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get configmap %s/%s: %w", namespace, name, err)
	}
	existing.Data = data
	existing.BinaryData = nil
	if _, err := cms.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update configmap %s/%s: %w", namespace, name, err)
	}
	return nil
}

// MergeConfigMap sets the given keys of the ConfigMap and keeps its other
// data, creating it when it does not exist: a failed sync's error written
// beside the previous manifest, which stays in place.
func MergeConfigMap(ctx context.Context, cs kubernetes.Interface, namespace, name string, data map[string]string) error {
	cms := cs.CoreV1().ConfigMaps(namespace)
	existing, err := cms.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return WriteConfigMap(ctx, cs, namespace, name, data)
	case err != nil:
		return fmt.Errorf("get configmap %s/%s: %w", namespace, name, err)
	}
	if existing.Data == nil {
		existing.Data = map[string]string{}
	}
	for k, v := range data {
		existing.Data[k] = v
	}
	if _, err := cms.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update configmap %s/%s: %w", namespace, name, err)
	}
	return nil
}
