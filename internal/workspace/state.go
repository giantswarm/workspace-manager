package workspace

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/mirror"
)

// ReadManifest is the last sync's manifest from the ConfigMap the
// Workspace's status names, read with the manager's ServiceAccount; nil
// before the first sync.
func ReadManifest(ctx context.Context, core kubernetes.Interface, namespace string, ws *v1alpha1.Workspace) (*mirror.Manifest, error) {
	if ws.Status.Manifest == "" {
		return nil, nil
	}
	cm, err := core.CoreV1().ConfigMaps(namespace).Get(ctx, ws.Status.Manifest, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the manifest of workspace %s: %w", ws.Name, err)
	}
	raw, ok := cm.Data[mirror.ManifestFile]
	if !ok {
		return nil, nil
	}
	var m mirror.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parse the manifest of workspace %s: %w", ws.Name, err)
	}
	return &m, nil
}

// Volume is the workspace's volume as its CSI driver knows it, read from its
// claim's bound PersistentVolume; while there is none, a reason instead: the
// claim missing, unbound or not a CSI volume.
func Volume(ctx context.Context, core kubernetes.Interface, namespace string, ws *v1alpha1.Workspace) (*v1alpha1.VolumeRef, string, error) {
	if ws.Status.Volume == nil || ws.Status.Volume.ClaimName == "" {
		return nil, "its volume is not claimed", nil
	}
	ref := &v1alpha1.VolumeRef{ClaimName: ws.Status.Volume.ClaimName}
	claim, err := core.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, ref.ClaimName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, fmt.Sprintf("its volume claim %s is gone", ref.ClaimName), nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read the volume claim of workspace %s: %w", ws.Name, err)
	}
	if claim.Spec.VolumeName == "" {
		return nil, fmt.Sprintf("its volume claim %s is not bound yet", claim.Name), nil
	}
	pv, err := core.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("read the volume of workspace %s: %w", ws.Name, err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver == "" || pv.Spec.CSI.VolumeHandle == "" {
		return nil, fmt.Sprintf("its volume %s is not a CSI volume, which a Session cannot mount", pv.Name), nil
	}
	ref.Driver, ref.Handle = pv.Spec.CSI.Driver, pv.Spec.CSI.VolumeHandle
	return ref, "", nil
}
