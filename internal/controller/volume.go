package controller

import (
	"context"
	"errors"
	"fmt"
	"math"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

// ErrNoStorageClass is returned while the installation names no StorageClass
// to claim workspace volumes from. A workspace volume is read-write-many and
// used by git, which few default classes serve, so the cluster's default
// class is never used in its place.
var ErrNoStorageClass = errors.New("no StorageClass to claim the workspace volume from: set --storage-class (the chart's storage.storageClassName) to the installation's read-write-many class; the cluster's default class is never used")

// NominalSize is what a claim requests before a sync measured the workspace.
// A class that provisions no size (EFS, an NFS export) ignores it; one that
// does gets the volume grown to the measured size.
var NominalSize = resource.MustParse("1Gi")

// LabelWorkspace labels what a Workspace owns with the Workspace's name.
const LabelWorkspace = "workspace-manager.giantswarm.io/workspace"

// ClaimName is the name of a Workspace's PersistentVolumeClaim, in the
// Workspace's namespace.
func ClaimName(ws *v1alpha1.Workspace) string { return "workspace-" + ws.Name }

// Sizing is the installation's rule for the size a volume is created with,
// where its StorageClass provisions one: the measured need times Factor, plus
// Headroom, capped at MaxSize.
type Sizing struct {
	// Factor multiplies the measured need; below 1 is refused by Validate.
	Factor float64
	// Headroom is added to the multiplied need; a Workspace's own larger
	// headroom replaces it.
	Headroom resource.Quantity
	// MaxSize caps the size; nil sets no ceiling.
	MaxSize *resource.Quantity
}

// DefaultSizing is the sizing without the installation's own.
var DefaultSizing = Sizing{Factor: 1.5, Headroom: resource.MustParse("5Gi")}

// Validate refuses a factor below 1, a negative headroom and a ceiling that
// is not positive.
func (s Sizing) Validate() error {
	if s.Factor < 1 || math.IsInf(s.Factor, 0) || math.IsNaN(s.Factor) {
		return fmt.Errorf("sizing factor must be a number of at least 1, got %v", s.Factor)
	}
	if s.Headroom.Sign() < 0 {
		return fmt.Errorf("sizing headroom must not be negative, got %s", s.Headroom.String())
	}
	if s.MaxSize != nil && s.MaxSize.Sign() <= 0 {
		return fmt.Errorf("sizing maximum size must be positive, got %s", s.MaxSize.String())
	}
	return nil
}

// Volumes claims each Workspace's read-write-many volume from one
// StorageClass, the installation's.
type Volumes struct {
	Client kubernetes.Interface
	// StorageClass is the class every claim names; empty refuses every claim
	// with ErrNoStorageClass.
	StorageClass string
	// Sizing sizes a new claim.
	Sizing Sizing
}

// Claim returns the Workspace's PersistentVolumeClaim, creating it when it
// does not exist: ReadWriteMany from the StorageClass, owned by the Workspace
// so it goes with it, sized by Size. An existing claim is returned as it is.
func (v Volumes) Claim(ctx context.Context, ws *v1alpha1.Workspace) (*corev1.PersistentVolumeClaim, error) {
	if v.StorageClass == "" {
		return nil, ErrNoStorageClass
	}
	pvcs := v.Client.CoreV1().PersistentVolumeClaims(ws.Namespace)
	name := ClaimName(ws)
	existing, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get claim %s/%s: %w", ws.Namespace, name, err)
	}
	class := v.StorageClass
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       ws.Namespace,
			Labels:          map[string]string{LabelWorkspace: ws.Name},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ws, v1alpha1.GroupVersion.WithKind("Workspace"))},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: v.Sizing.Size(ws)},
			},
		},
	}
	created, err := pvcs.Create(ctx, pvc, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		// Another replica claimed it between the get and the create.
		if existing, err = pvcs.Get(ctx, name, metav1.GetOptions{}); err != nil {
			return nil, fmt.Errorf("get claim %s/%s: %w", ws.Namespace, name, err)
		}
		return existing, nil
	}
	if err != nil {
		return nil, fmt.Errorf("create claim %s/%s from StorageClass %q: %w", ws.Namespace, name, class, err)
	}
	return created, nil
}

// Size is the size a Workspace's volume is claimed with: the provisioned size
// the status reports; else the measured need times the factor plus the
// headroom (the larger of the installation's and the Workspace's), or
// NominalSize while nothing is measured; at least the spec's minimum; capped
// at MaxSize, which wins over the minimum.
func (s Sizing) Size(ws *v1alpha1.Workspace) resource.Quantity {
	if p := ws.Status.VolumeSize; p != nil && p.Sign() > 0 {
		return *p
	}
	size := NominalSize.DeepCopy()
	if need := ws.Status.NeededSize; need != nil && need.Sign() > 0 {
		headroom := s.Headroom
		if ws.Spec.Sizing != nil && ws.Spec.Sizing.Headroom != nil && ws.Spec.Sizing.Headroom.Cmp(headroom) > 0 {
			headroom = *ws.Spec.Sizing.Headroom
		}
		size = *resource.NewQuantity(int64(math.Ceil(need.AsApproximateFloat64()*s.Factor)), resource.BinarySI)
		size.Add(headroom)
	}
	if ws.Spec.Sizing != nil && ws.Spec.Sizing.Minimum != nil && ws.Spec.Sizing.Minimum.Cmp(size) > 0 {
		size = ws.Spec.Sizing.Minimum.DeepCopy()
	}
	if s.MaxSize != nil && size.Cmp(*s.MaxSize) > 0 {
		size = s.MaxSize.DeepCopy()
	}
	return size
}
