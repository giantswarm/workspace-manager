// Package controller is the manager's controller: leader-elected among the
// replicas, it watches the Workspaces of one namespace and runs each one's
// sync cycle (every change, every sync request): it lists the
// workspace's owners, claims and sizes its read-write-many volume from the
// installation's StorageClass, starts the sync Job when something changed
// and records the outcome in the Workspace's status, which nothing else
// writes.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/workqueue"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

// DefaultResync is how often every Workspace is reconciled again without an
// event, and how long after a failed cycle the next one is tried.
const DefaultResync = 10 * time.Minute

// DefaultSessionCleanupAfter is how long a Session's directory outlives the
// Session's last turn: 14 days, the platform's Session idle TTL.
const DefaultSessionCleanupAfter = 14 * 24 * time.Hour

// DeletionRecheck is how long after its last cycle a deleted workspace whose
// session directories have no earlier cleanup date is looked after again.
const DeletionRecheck = 24 * time.Hour

// DefaultSessionPoll is how often kagent's Sessions are listed for the ones
// deleted since the last sync, whose directories go at once.
const DefaultSessionPoll = 30 * time.Second

// DefaultLease is the Lease the replicas elect the controller with.
const DefaultLease = "workspace-manager-controller"

// LeaderElection is how the replicas elect the one running the controller: a
// Lease in the workspaces' namespace.
type LeaderElection struct {
	// Lease is the Lease's name; empty is DefaultLease.
	Lease string
	// Identity is this replica's, the pod's name.
	Identity string
	// LeaseDuration, RenewDeadline and RetryPeriod are the election's
	// timings; zero takes 15s, 10s and 2s.
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
}

// ErrLeadershipLost is returned by Run when the replica lost the election
// while it ran the controller: the process restarts and competes again.
var ErrLeadershipLost = errors.New("leadership lost")

// Config is what the controller works with.
type Config struct {
	// Dynamic reads and watches the Workspaces and writes their status.
	Dynamic dynamic.Interface
	// Core writes what a Workspace owns: its claim, its sync Jobs and their
	// input.
	Core kubernetes.Interface
	// Namespace is where the Workspaces and everything they own live.
	Namespace string
	// StorageClass is the read-write-many class every workspace volume is
	// claimed from; empty claims none and every Workspace's VolumeClaimed
	// condition names the missing value.
	StorageClass string
	// Resync is how often every Workspace is reconciled without an event
	// and how long after a failed cycle it is tried again (the chart's
	// sync.cycle); zero is DefaultResync.
	Resync time.Duration
	// Sizing sizes each volume; the zero value is DefaultSizing.
	Sizing Sizing
	// SessionCleanupAfter is how long a Session's directory outlives the
	// Session's last turn (the chart's sessions.cleanupAfter); zero is
	// DefaultSessionCleanupAfter.
	SessionCleanupAfter time.Duration
	// SessionPoll is how often kagent's Sessions are listed, once for every
	// workspace, for a Session deleted since the last sync: its directory
	// goes in a cleanup-only Job (the chart's sessions.poll); zero is
	// DefaultSessionPoll.
	SessionPoll time.Duration
	// Sessions lists kagent's Sessions, whose last turns the session
	// directories' cleanup works from; nil cleans up no directory.
	Sessions Sessions
	// Listings resolve a workspace's sources; nil fails every cycle.
	Listings Listings
	// Instances are the installation's provider instances, whose sync
	// credentials the Job fetches with, read through Secrets.
	Instances []provider.Instance
	Secrets   provider.Secrets
	// SyncJobTemplate is the Job every cycle's Job is made from; nil is
	// DefaultJobTemplate on SyncServiceAccount.
	SyncJobTemplate    *batchv1.Job
	SyncServiceAccount string
	// SyncImage runs the sync Job, the manager's own image; empty keeps the
	// template's.
	SyncImage           string
	SyncImagePullPolicy corev1.PullPolicy
	// SyncRoots is the PEM bundle the sync Job verifies the providers' git
	// hosts with, the manager's own (Jobs.Roots); empty hands none.
	SyncRoots []byte
	// LeaderElection elects the replica running the controller; nil runs it
	// in this one.
	LeaderElection *LeaderElection
	// Now is the clock; nil is time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Controller reconciles the Workspaces of one namespace.
type Controller struct {
	dynamic   dynamic.Interface
	core      kubernetes.Interface
	namespace string
	resync    time.Duration
	volumes   Volumes
	sizing    Sizing
	jobs      Jobs
	listings  Listings
	sessions  Sessions
	instances map[string]provider.Instance
	secrets   provider.Secrets
	status    *Status
	election  *LeaderElection
	// sessionCleanupAfter is the session directories' cleanup window.
	sessionCleanupAfter time.Duration
	// sessionPoll is how often a workspace with session directories checks
	// kagent's listing for a deleted Session, listed reads it.
	sessionPoll time.Duration
	listed      listedSessions
	now         func() time.Time
	log         *slog.Logger
	leading     atomic.Bool
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
	if cfg.SessionPoll == 0 {
		cfg.SessionPoll = DefaultSessionPoll
	}
	if cfg.SyncJobTemplate == nil {
		cfg.SyncJobTemplate = DefaultJobTemplate(cfg.SyncServiceAccount)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Listings == nil {
		cfg.Listings = noListings{}
	}
	instances := make(map[string]provider.Instance, len(cfg.Instances))
	for _, in := range cfg.Instances {
		instances[in.Name] = in
	}
	return &Controller{
		dynamic:             cfg.Dynamic,
		core:                cfg.Core,
		namespace:           cfg.Namespace,
		resync:              cfg.Resync,
		volumes:             Volumes{Client: cfg.Core, StorageClass: cfg.StorageClass},
		sizing:              cfg.Sizing,
		jobs:                Jobs{Client: cfg.Core, Template: cfg.SyncJobTemplate, Image: cfg.SyncImage, ImagePullPolicy: cfg.SyncImagePullPolicy, Roots: cfg.SyncRoots},
		listings:            cfg.Listings,
		sessions:            cfg.Sessions,
		instances:           instances,
		secrets:             cfg.Secrets,
		status:              NewStatus(cfg.Dynamic, cfg.Namespace),
		election:            cfg.LeaderElection,
		sessionCleanupAfter: cfg.SessionCleanupAfter,
		sessionPoll:         cfg.SessionPoll,
		now:                 cfg.Now,
		log:                 cfg.Logger,
	}
}

// noListings fails every cycle: the manager runs without its providers'
// Secrets.
type noListings struct{}

func (noListings) Resolve(context.Context, []v1alpha1.Source) (*workspace.Selection, error) {
	return nil, errors.New("no listing: the manager runs without --manager-namespace (POD_NAMESPACE), where the providers' Secrets are")
}

// Leading reports whether this replica runs the controller.
func (c *Controller) Leading() bool { return c.leading.Load() }

// Run elects this replica and runs the controller while it leads, until ctx
// ends. Without leader election it runs at once. A replica that lost the
// election returns ErrLeadershipLost: the process restarts and competes
// again.
func (c *Controller) Run(ctx context.Context) error {
	if c.election == nil {
		c.leading.Store(true)
		defer c.leading.Store(false)
		return c.run(ctx)
	}
	e := *c.election
	if e.Lease == "" {
		e.Lease = DefaultLease
	}
	if e.LeaseDuration == 0 {
		e.LeaseDuration, e.RenewDeadline, e.RetryPeriod = 15*time.Second, 10*time.Second, 2*time.Second
	}
	var (
		runErr error
		done   = make(chan struct{})
		led    atomic.Bool
	)
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: &resourcelock.LeaseLock{
			LeaseMeta:  metav1.ObjectMeta{Name: e.Lease, Namespace: c.namespace},
			Client:     c.core.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{Identity: e.Identity},
		},
		LeaseDuration:   e.LeaseDuration,
		RenewDeadline:   e.RenewDeadline,
		RetryPeriod:     e.RetryPeriod,
		ReleaseOnCancel: true,
		Name:            e.Lease,
		Callbacks: leaderelection.LeaderCallbacks{
			// The elector cancels leadCtx when the lease is lost or ctx ends,
			// which stops the loop; the loop's end closes done.
			OnStartedLeading: func(leadCtx context.Context) {
				led.Store(true)
				c.leading.Store(true)
				c.log.Info("controller leading", "lease", e.Lease, "identity", e.Identity)
				runErr = c.run(leadCtx)
				close(done)
			},
			OnStoppedLeading: func() {
				c.leading.Store(false)
				c.log.Info("controller stopped leading", "lease", e.Lease, "identity", e.Identity)
			},
			OnNewLeader: func(identity string) {
				if identity != e.Identity {
					c.log.Info("controller led by another replica", "lease", e.Lease, "leader", identity)
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("leader election: %w", err)
	}
	// Returns once ctx ends or, while leading, the lease is lost.
	elector.Run(ctx)
	if !led.Load() {
		return nil
	}
	<-done
	if runErr != nil {
		return runErr
	}
	if ctx.Err() == nil {
		return ErrLeadershipLost
	}
	return nil
}

// run watches the Workspaces and their sync Jobs and reconciles each
// Workspace on every change, every resync, every Job event and when a
// session directory's cleanup date comes, until ctx ends. A reconcile that failed on the API
// server is retried with backoff.
func (c *Controller) run(ctx context.Context) error {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	enqueue := func(obj any) {
		if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
			queue.Add(key)
		}
	}
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(c.dynamic, c.resync, c.namespace, nil)
	informer := factory.ForResource(v1alpha1.WorkspaceResource).Informer()
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    enqueue,
		UpdateFunc: func(_, obj any) { enqueue(obj) },
		// A deleted Workspace needs nothing: what it owns goes with it, by
		// the owner references.
	}); err != nil {
		return fmt.Errorf("watch workspaces: %w", err)
	}
	// A sync Job's end wakes its Workspace.
	jobFactory := informers.NewSharedInformerFactoryWithOptions(c.core, c.resync, informers.WithNamespace(c.namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = LabelWorkspace }))
	jobInformer := jobFactory.Batch().V1().Jobs().Informer()
	enqueueOwner := func(obj any) {
		if job, ok := obj.(*batchv1.Job); ok {
			if ws := job.Labels[LabelWorkspace]; ws != "" {
				queue.Add(c.namespace + "/" + ws)
			}
		}
	}
	if _, err := jobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, obj any) { enqueueOwner(obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			enqueueOwner(obj)
		},
	}); err != nil {
		return fmt.Errorf("watch sync jobs: %w", err)
	}
	factory.Start(ctx.Done())
	defer factory.Shutdown()
	jobFactory.Start(ctx.Done())
	defer jobFactory.Shutdown()
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced, jobInformer.HasSynced) {
		return nil
	}
	c.log.Info("controller started", "storageClass", c.volumes.StorageClass, "sizingFactor", c.sizing.Factor,
		"sizingHeadroom", c.sizing.HeadroomFloor.String(), "sizingHeadroomShare", c.sizing.HeadroomShare,
		"sizingMaxSize", quantityString(c.sizing.MaxSize), "resync", c.resync, "sessionCleanupAfter", c.sessionCleanupAfter, "sessionPoll", c.sessionPoll)
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

// process reconciles one queued Workspace, read live rather than from the
// informer's cache: a key queued again (a resync, a Job event) while its
// reconcile wrote the status would otherwise be served the status before the
// write and start the cycle a second time.
func (c *Controller) process(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string], key string) {
	defer queue.Done(key)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		queue.Forget(key)
		return
	}
	u, err := c.dynamic.Resource(v1alpha1.WorkspaceResource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		queue.Forget(key)
		return
	}
	if err != nil {
		c.log.Warn("workspace not read", "workspace", key, "error", err)
		queue.AddRateLimited(key)
		return
	}
	ws, err := fromUnstructured(u)
	if err != nil {
		c.log.Warn("workspace not reconciled", "workspace", key, "error", err)
		queue.Forget(key)
		return
	}
	requeue, err := c.Reconcile(ctx, ws)
	if err != nil {
		c.log.Warn("workspace not reconciled", "workspace", key, "error", err)
		queue.AddRateLimited(key)
		return
	}
	queue.Forget(key)
	if requeue > 0 {
		queue.AddAfter(key, requeue)
	}
}

// Reconcile runs the Workspace's sync cycle when one is due, or records the
// outcome of the Job of the running one, and says how long until the next
// one is due (zero for none). No cycle starts while a sync Job of the
// Workspace runs, named in the status or not. An error is the API server's: the caller
// retries with backoff. With no cycle due, a Session deleted in kagent has
// its directory deleted at once, and a directory past its cleanup date goes
// at that date (housekeep). A Workspace being deleted
// syncs no more; its cycles only clean up its session directories until the
// last is gone (reconcileDeleted).
func (c *Controller) Reconcile(ctx context.Context, ws *v1alpha1.Workspace) (time.Duration, error) {
	if ws.DeletionTimestamp != nil {
		return c.reconcileDeleted(ctx, ws)
	}
	if err := c.ensureFinalizer(ctx, ws); err != nil {
		return 0, err
	}
	if ws.Status.ActiveSync != nil {
		done, err := c.finish(ctx, ws)
		if err != nil || !done {
			return 0, err
		}
		// The cycle is over; a request or a change made during it starts
		// the next one now.
		if ws, err = c.get(ctx, ws); err != nil {
			return 0, err
		}
	}
	if running, err := c.syncRunning(ctx, ws); err != nil || running {
		return 0, err
	}
	if ws.Status.StoredSize == nil && ws.Status.Manifest != "" {
		if err := c.recordStored(ctx, ws); err != nil {
			return 0, err
		}
	}
	claim, err := c.volumes.Get(ctx, ws)
	if err != nil {
		return 0, err
	}
	now := c.now()
	trigger := c.trigger(ws, now, claim)
	if trigger == "" {
		return c.housekeep(ctx, ws, now, claim)
	}
	return c.cycle(ctx, ws, trigger, claim)
}

// syncRunning reports whether a sync Job of the Workspace runs that its
// status does not name: no cycle starts beside it on the same volume, and
// its end wakes the Workspace for the next one, which serves every trigger
// that came meanwhile.
func (c *Controller) syncRunning(ctx context.Context, ws *v1alpha1.Workspace) (bool, error) {
	job, err := c.jobs.Running(ctx, ws)
	if err != nil || job == nil {
		return false, err
	}
	c.log.Warn("sync job runs that the status does not name: the next cycle waits for its end", "workspace", ws.Name, "job", job.Name,
		"hint", "a status write lost after the Job started, or a Workspace CRD older than the manager that prunes status.activeSync")
	return true, nil
}

// get reads the Workspace anew.
func (c *Controller) get(ctx context.Context, ws *v1alpha1.Workspace) (*v1alpha1.Workspace, error) {
	u, err := c.dynamic.Resource(v1alpha1.WorkspaceResource).Namespace(ws.Namespace).Get(ctx, ws.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get workspace %q: %w", ws.Name, err)
	}
	return fromUnstructured(u)
}
