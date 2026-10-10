// Package controller is the manager's controller: it watches the Workspaces
// of one namespace and keeps what each one owns, for now its read-write-many
// volume, claimed from the installation's StorageClass, and writes the
// Workspace's status, which nothing else writes. It runs in every replica:
// each operation is idempotent and a status write that changes nothing is
// not made.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
)

// DefaultResync is how often every Workspace is reconciled again without an
// event: a claim deleted out from under a Workspace is made again by then.
const DefaultResync = 10 * time.Minute

// DefaultSessionCleanupAfter is how long a Session's directory outlives the
// Session's last turn: 30 days.
const DefaultSessionCleanupAfter = 30 * 24 * time.Hour

// Config is what the controller works with.
type Config struct {
	// Dynamic reads and watches the Workspaces and writes their status.
	Dynamic dynamic.Interface
	// Core writes what a Workspace owns: its claim.
	Core kubernetes.Interface
	// Namespace is where the Workspaces and everything they own live.
	Namespace string
	// StorageClass is the read-write-many class every workspace volume is
	// claimed from; empty claims none and every Workspace's VolumeClaimed
	// condition names the missing value.
	StorageClass string
	// Resync is how often every Workspace is reconciled without an event
	// (the chart's sync.cycle); zero is DefaultResync.
	Resync time.Duration
	// Sizing sizes each new claim; the zero value is DefaultSizing.
	Sizing Sizing
	// SessionCleanupAfter is how long a Session's directory outlives the
	// Session's last turn (the chart's sessions.cleanupAfter); zero is
	// DefaultSessionCleanupAfter.
	SessionCleanupAfter time.Duration
	Logger              *slog.Logger
}

// Controller reconciles the Workspaces of one namespace.
type Controller struct {
	factory  dynamicinformer.DynamicSharedInformerFactory
	informer cache.SharedIndexInformer
	volumes  Volumes
	status   *Status
	// sessionCleanupAfter is the session directories' cleanup window.
	sessionCleanupAfter time.Duration
	log                 *slog.Logger
}

// New builds the controller; Run starts it.
func New(cfg Config) *Controller {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Resync == 0 {
		cfg.Resync = DefaultResync
	}
	if cfg.Sizing == (Sizing{}) {
		cfg.Sizing = DefaultSizing
	}
	if cfg.SessionCleanupAfter == 0 {
		cfg.SessionCleanupAfter = DefaultSessionCleanupAfter
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(cfg.Dynamic, cfg.Resync, cfg.Namespace, nil)
	return &Controller{
		factory:             factory,
		informer:            factory.ForResource(v1alpha1.WorkspaceResource).Informer(),
		volumes:             Volumes{Client: cfg.Core, StorageClass: cfg.StorageClass, Sizing: cfg.Sizing},
		status:              NewStatus(cfg.Dynamic, cfg.Namespace),
		sessionCleanupAfter: cfg.SessionCleanupAfter,
		log:                 cfg.Logger,
	}
}

// Run watches the Workspaces and reconciles each one on every change and
// every resync, until ctx ends. A reconcile that failed is retried with
// backoff; one refused for the missing StorageClass is not, since only a
// restart with the flag changes that.
func (c *Controller) Run(ctx context.Context) error {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	enqueue := func(obj any) {
		if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
			queue.Add(key)
		}
	}
	if _, err := c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueue,
		UpdateFunc: func(_, obj any) { enqueue(obj) },
		// A deleted Workspace needs nothing: its claim goes with it, by the
		// owner reference.
	}); err != nil {
		return fmt.Errorf("watch workspaces: %w", err)
	}
	c.factory.Start(ctx.Done())
	defer c.factory.Shutdown()
	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		return nil
	}
	c.log.Info("controller started", "storageClass", c.volumes.StorageClass, "sizingFactor", c.volumes.Sizing.Factor,
		"sizingHeadroom", c.volumes.Sizing.Headroom.String(), "sizingMaxSize", maxSizeString(c.volumes.Sizing.MaxSize),
		"sessionCleanupAfter", c.sessionCleanupAfter)
	go func() {
		<-ctx.Done()
		queue.ShutDown()
	}()
	for {
		key, shutdown := queue.Get()
		if shutdown {
			return nil
		}
		c.process(ctx, queue, key)
	}
}

func (c *Controller) process(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string], key string) {
	defer queue.Done(key)
	obj, exists, err := c.informer.GetIndexer().GetByKey(key)
	if err != nil {
		c.log.Warn("workspace not read", "workspace", key, "error", err)
		queue.AddRateLimited(key)
		return
	}
	if !exists {
		queue.Forget(key)
		return
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		queue.Forget(key)
		return
	}
	ws, err := fromUnstructured(u)
	if err != nil {
		c.log.Warn("workspace not reconciled", "workspace", key, "error", err)
		queue.Forget(key)
		return
	}
	switch err := c.Reconcile(ctx, ws); {
	case err == nil:
		queue.Forget(key)
	case errors.Is(err, ErrNoStorageClass):
		c.log.Warn("workspace volume not claimed", "workspace", key, "reason", v1alpha1.ReasonStorageClassMissing)
		queue.Forget(key)
	default:
		c.log.Warn("workspace not reconciled", "workspace", key, "error", err)
		queue.AddRateLimited(key)
	}
}

// Reconcile claims the Workspace's volume and records the outcome in its
// status: the claim's name and the VolumeClaimed condition, which names the
// missing StorageClass while there is none to claim from. It returns the
// claim's error, ErrNoStorageClass included, after the status is written.
func (c *Controller) Reconcile(ctx context.Context, ws *v1alpha1.Workspace) error {
	claim, err := c.volumes.Claim(ctx, ws)
	cond := metav1.Condition{Type: v1alpha1.ConditionVolumeClaimed, ObservedGeneration: ws.Generation}
	switch {
	case err == nil:
		cond.Status, cond.Reason = metav1.ConditionTrue, v1alpha1.ReasonVolumeClaimed
		cond.Message = fmt.Sprintf("claim %s from StorageClass %s", claim.Name, ptr.Deref(claim.Spec.StorageClassName, ""))
	case errors.Is(err, ErrNoStorageClass):
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, v1alpha1.ReasonStorageClassMissing, err.Error()
	default:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, v1alpha1.ReasonVolumeClaimFailed, err.Error()
	}
	if werr := c.status.Update(ctx, ws.Name, func(w *v1alpha1.Workspace) bool {
		changed := meta.SetStatusCondition(&w.Status.Conditions, cond)
		if claim != nil && (w.Status.Volume == nil || w.Status.Volume.ClaimName != claim.Name) {
			if w.Status.Volume == nil {
				w.Status.Volume = &v1alpha1.VolumeRef{}
			}
			w.Status.Volume.ClaimName = claim.Name
			changed = true
		}
		return changed
	}); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}

// maxSizeString is the sizing ceiling as logged: "none" without one.
func maxSizeString(q *resource.Quantity) string {
	if q == nil {
		return "none"
	}
	return q.String()
}
