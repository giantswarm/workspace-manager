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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

// ErrNoStorageClass is returned while the installation names no StorageClass
// to claim workspace volumes from. A workspace volume is read-write-many and
// used by git, which few default classes serve, so the cluster's default
// class is never used in its place.
var ErrNoStorageClass = errors.New("no StorageClass to claim the workspace volume from: set --storage-class (the chart's storage.storageClassName) to the installation's read-write-many class; the cluster's default class is never used")

// NominalSize is what a claim requests on a class that provisions no size
// (EFS, an NFS export): the request is ignored there, and nothing is sized.
var NominalSize = resource.MustParse("1Gi")

// LabelWorkspace labels what a Workspace owns with the Workspace's name.
const LabelWorkspace = "workspace-manager.giantswarm.io/workspace"

// GiB is one gibibyte, the unit a volume's size is rounded up to.
const GiB = int64(1) << 30

// ClaimName is the name of a Workspace's PersistentVolumeClaim, in the
// Workspace's namespace.
func ClaimName(ws *v1alpha1.Workspace) string { return "workspace-" + ws.Name }

// Sizing is the installation's rule for the size of a workspace's volume,
// where its StorageClass provisions one: the need plus a headroom, rounded up
// to the GiB.
//
//	need     = the mirrors (a repository not yet mirrored counts Factor × the
//	           size its provider reports; a mirrored one its measured size)
//	           plus the live session directories
//	headroom = max(HeadroomFloor, HeadroomShare × need, the Workspace's own)
//	size     = max(the Workspace's minimum, need + headroom), whole GiB
//
// MaxSize is the installation's optional ceiling: a cycle that would pass
// it fails, naming the size it needs.
type Sizing struct {
	// Factor multiplies a provider's reported size for a repository not yet
	// mirrored; below 1 is refused by Validate.
	Factor float64
	// HeadroomFloor is the least headroom.
	HeadroomFloor resource.Quantity
	// HeadroomShare is the headroom as a share of the need; negative is
	// refused by Validate.
	HeadroomShare float64
	// MaxSize caps the size; nil sets no ceiling.
	MaxSize *resource.Quantity
}

// DefaultSizing is the sizing without the installation's own: 1.5 × the
// reported size for an unmirrored repository, a headroom of 4Gi or half the
// need, no ceiling.
var DefaultSizing = Sizing{Factor: 1.5, HeadroomFloor: resource.MustParse("4Gi"), HeadroomShare: 0.5}

// Validate refuses a factor below 1, a negative headroom floor or share and a
// ceiling that is not positive.
func (s Sizing) Validate() error {
	if s.Factor < 1 || math.IsInf(s.Factor, 0) || math.IsNaN(s.Factor) {
		return fmt.Errorf("sizing factor must be a number of at least 1, got %v", s.Factor)
	}
	if s.HeadroomFloor.Sign() < 0 {
		return fmt.Errorf("sizing headroom must not be negative, got %s", s.HeadroomFloor.String())
	}
	if s.HeadroomShare < 0 || math.IsInf(s.HeadroomShare, 0) || math.IsNaN(s.HeadroomShare) {
		return fmt.Errorf("sizing headroom share must be a non-negative number, got %v", s.HeadroomShare)
	}
	if s.MaxSize != nil && s.MaxSize.Sign() <= 0 {
		return fmt.Errorf("sizing maximum size must be positive, got %s", s.MaxSize.String())
	}
	return nil
}

// Need is what a workspace's volume must hold, in bytes: the measured mirrors
// the manifest records (dropped ones included, they are on the volume until
// the cleanup removes them), the estimate for every selected repository the
// manifest does not record, and the live session directories.
func (s Sizing) Need(manifest *mirror.Manifest, selected []provider.Item, sessions []v1alpha1.SessionDirectory) int64 {
	var need int64
	mirrored := map[string]bool{}
	if manifest != nil {
		for _, e := range manifest.Repositories {
			mirrored[e.Key()] = true
			need += e.SizeBytes
		}
	}
	for _, it := range selected {
		if !mirrored[it.Key()] {
			need += s.Estimate(it)
		}
	}
	for _, d := range sessions {
		if d.Size != nil {
			need += d.Size.Value()
		}
	}
	return need
}

// Estimate is what a repository not yet mirrored counts: Factor × the size
// its provider reports.
func (s Sizing) Estimate(it provider.Item) int64 {
	return int64(math.Ceil(float64(it.SizeKiB*1024) * s.Factor))
}

// Size is the volume's size for a need, the Workspace's own headroom and
// minimum applied, rounded up to the whole GiB. The ceiling is Exceeds'.
func (s Sizing) Size(need int64, spec *v1alpha1.Sizing) resource.Quantity {
	headroom := max(s.HeadroomFloor.Value(), int64(math.Ceil(float64(need)*s.HeadroomShare)))
	if spec != nil && spec.Headroom != nil {
		headroom = max(headroom, spec.Headroom.Value())
	}
	size := need + headroom
	if spec != nil && spec.Minimum != nil {
		size = max(size, spec.Minimum.Value())
	}
	return *resource.NewQuantity(roundUpGiB(size), resource.BinarySI)
}

// Exceeds reports whether size passes the ceiling, with the refusal naming
// both.
func (s Sizing) Exceeds(size resource.Quantity) error {
	if s.MaxSize == nil || size.Cmp(*s.MaxSize) <= 0 {
		return nil
	}
	return fmt.Errorf("the volume needs %s, past the installation's ceiling of %s", size.String(), s.MaxSize.String())
}

func roundUpGiB(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return (bytes + GiB - 1) / GiB * GiB
}

// Volumes claims each Workspace's read-write-many volume from one
// StorageClass, the installation's, and grows it.
type Volumes struct {
	Client kubernetes.Interface
	// StorageClass is the class every claim names; empty refuses every claim
	// with ErrNoStorageClass.
	StorageClass string
}

// Provisioned reports whether the StorageClass provisions a size: a class
// that allows volume expansion is sized by the formula and grown (Azure
// Files, an NFS server on block storage); one that does not is elastic
// (EFS), and nothing is sized on it.
func (v Volumes) Provisioned(ctx context.Context) (bool, error) {
	if v.StorageClass == "" {
		return false, ErrNoStorageClass
	}
	sc, err := v.Client.StorageV1().StorageClasses().Get(ctx, v.StorageClass, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("read StorageClass %s: %w", v.StorageClass, err)
	}
	return sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion, nil
}

// Get returns the Workspace's claim, nil when there is none.
func (v Volumes) Get(ctx context.Context, ws *v1alpha1.Workspace) (*corev1.PersistentVolumeClaim, error) {
	claim, err := v.Client.CoreV1().PersistentVolumeClaims(ws.Namespace).Get(ctx, ClaimName(ws), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get claim %s/%s: %w", ws.Namespace, ClaimName(ws), err)
	}
	return claim, nil
}

// Claim returns the Workspace's PersistentVolumeClaim, creating it when it
// does not exist: ReadWriteMany from the StorageClass, owned by the Workspace
// so it goes with it, requesting size. An existing claim is returned as it is.
func (v Volumes) Claim(ctx context.Context, ws *v1alpha1.Workspace, size resource.Quantity) (*corev1.PersistentVolumeClaim, error) {
	if v.StorageClass == "" {
		return nil, ErrNoStorageClass
	}
	existing, err := v.Get(ctx, ws)
	if err != nil || existing != nil {
		return existing, err
	}
	pvcs := v.Client.CoreV1().PersistentVolumeClaims(ws.Namespace)
	name := ClaimName(ws)
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
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
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

// Grow raises the claim's request to size when it is below it; a claim is
// never shrunk, since the volume holds live session directories. It returns
// the request in effect.
func (v Volumes) Grow(ctx context.Context, claim *corev1.PersistentVolumeClaim, size resource.Quantity) (resource.Quantity, error) {
	current := claim.Spec.Resources.Requests[corev1.ResourceStorage]
	if current.Cmp(size) >= 0 {
		return current, nil
	}
	patch := fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":%q}}}}`, size.String())
	if _, err := v.Client.CoreV1().PersistentVolumeClaims(claim.Namespace).Patch(ctx, claim.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return current, fmt.Errorf("grow claim %s/%s to %s: %w", claim.Namespace, claim.Name, size.String(), err)
	}
	return size, nil
}

// Requested is the claim's storage request.
func Requested(claim *corev1.PersistentVolumeClaim) resource.Quantity {
	return claim.Spec.Resources.Requests[corev1.ResourceStorage]
}
