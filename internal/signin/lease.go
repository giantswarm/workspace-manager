package signin

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// refreshLease is the cross-replica lock around one sign-in's refresh: a
// coordination.k8s.io Lease named like the sign-in's Secret. A holder that
// died is taken over once its lease ran out.
type refreshLease struct {
	client    kubernetes.Interface
	namespace string
	identity  string
	duration  time.Duration
	poll      time.Duration
	now       func() time.Time
}

// acquire takes the lease. While another replica holds it, it calls done
// between attempts and gives up without the lease once done reports true
// (the holder finished the work). held says whether the lease was taken.
func (l *refreshLease) acquire(ctx context.Context, name string, labels map[string]string, done func() (bool, error)) (held bool, err error) {
	for {
		taken, err := l.try(ctx, name, labels)
		if err != nil {
			return false, err
		}
		if taken {
			return true, nil
		}
		finished, err := done()
		if err != nil || finished {
			return false, err
		}
		// Jittered, so waiting replicas do not retry in lockstep.
		wait := l.poll/2 + rand.N(l.poll) //nolint:gosec // jitter, not a secret
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("wait for refresh lease %s: %w", name, ctx.Err())
		case <-time.After(wait):
		}
	}
}

// try takes the lease once: creates it, or takes it over when it is free,
// expired or already this replica's. Losing a race is (false, nil).
func (l *refreshLease) try(ctx context.Context, name string, labels map[string]string) (bool, error) {
	leases := l.client.CoordinationV1().Leases(l.namespace)
	now := metav1.NewMicroTime(l.now())
	lease, err := leases.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = leases.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: l.namespace, Labels: labels},
			Spec:       l.spec(now),
		}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("create refresh lease %s: %w", name, err)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read refresh lease %s: %w", name, err)
	}
	if !l.free(lease) {
		return false, nil
	}
	lease.Spec = l.spec(now)
	_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("take refresh lease %s: %w", name, err)
	}
	return true, nil
}

// release deletes the lease if this replica still holds it; the
// preconditions make sure a lease another replica took over stays.
func (l *refreshLease) release(ctx context.Context, name string) error {
	leases := l.client.CoordinationV1().Leases(l.namespace)
	lease, err := leases.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read refresh lease %s: %w", name, err)
	}
	if ptr.Deref(lease.Spec.HolderIdentity, "") != l.identity {
		return nil
	}
	err = leases.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID:             &lease.UID,
		ResourceVersion: &lease.ResourceVersion,
	}})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("release refresh lease %s: %w", name, err)
	}
	return nil
}

func (l *refreshLease) free(lease *coordinationv1.Lease) bool {
	holder := ptr.Deref(lease.Spec.HolderIdentity, "")
	if holder == "" || holder == l.identity {
		return true
	}
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	expiry := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
	return l.now().After(expiry)
}

func (l *refreshLease) spec(now metav1.MicroTime) coordinationv1.LeaseSpec {
	return coordinationv1.LeaseSpec{
		HolderIdentity:       ptr.To(l.identity),
		LeaseDurationSeconds: ptr.To(int32(l.duration / time.Second)), //nolint:gosec // NewKubeStore bounds it to an hour
		AcquireTime:          &now,
		RenewTime:            &now,
	}
}
