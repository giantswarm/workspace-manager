package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

// Listings resolves a workspace's sources to the repositories they select
// now, one listing per owner with the provider's own sync credential
// (workspace.Listings).
type Listings interface {
	Resolve(ctx context.Context, sources []v1alpha1.Source) (*workspace.Selection, error)
}

// trigger is what starts a cycle now, empty for nothing: a sync request, a
// changed spec, a volume claimed anew, a first sync, a retry of a failed
// cycle once the resync has passed, or the schedule come due. A cycle that
// failed less than a resync ago starts again only for a request or a change.
func (c *Controller) trigger(ws *v1alpha1.Workspace, now time.Time, claim *corev1.PersistentVolumeClaim) v1alpha1.SyncTrigger {
	if ws.Annotations[v1alpha1.AnnotationSyncRequested] != "" {
		return v1alpha1.TriggerRequest
	}
	if ws.Status.ObservedGeneration != ws.Generation {
		return v1alpha1.TriggerChange
	}
	last := ws.Status.LastSync
	if last == nil {
		return v1alpha1.TriggerChange
	}
	if last.Result == v1alpha1.SyncFailed {
		if now.Before(last.Time.Add(c.resync)) {
			return ""
		}
		return v1alpha1.TriggerRetry
	}
	if claim == nil {
		return v1alpha1.TriggerChange
	}
	if next := workspace.NextSync(ws.Spec.Sync, last.Time.Time); !next.IsZero() && !next.After(now) {
		return v1alpha1.TriggerSchedule
	}
	return ""
}

// wake is how long until the next cycle is due, zero for none: the schedule
// after the last cycle, or the retry of a failed one.
func (c *Controller) wake(ws *v1alpha1.Workspace, now time.Time) time.Duration {
	last := ws.Status.LastSync
	if last == nil || ws.Status.ActiveSync != nil {
		return 0
	}
	var at time.Time
	if last.Result == v1alpha1.SyncFailed {
		at = last.Time.Add(c.resync)
	} else {
		at = workspace.NextSync(ws.Spec.Sync, last.Time.Time)
	}
	if at.IsZero() {
		return 0
	}
	return max(at.Sub(now), time.Second)
}

// cycle runs one sync cycle: the request annotations go, each owner is
// listed once, the selection is compared with the last manifest, the volume
// is claimed or grown to the formula's size where its class provisions one,
// kagent's Sessions on the volume are listed for the session directories'
// cleanup, and the sync Job starts unless nothing changed and there is no
// session directory or dropped mirror to look after. A failure of the
// listing, the sizing or the claim is recorded and tried again at the next
// resync.
func (c *Controller) cycle(ctx context.Context, ws *v1alpha1.Workspace, trigger v1alpha1.SyncTrigger, claim *corev1.PersistentVolumeClaim) (time.Duration, error) {
	log := c.log.With("workspace", ws.Name, "trigger", trigger)
	if trigger == v1alpha1.TriggerRequest {
		log = log.With("requestedAt", ws.Annotations[v1alpha1.AnnotationSyncRequested], "requestedBy", ws.Annotations[v1alpha1.AnnotationSyncRequestedBy])
		if err := c.clearRequest(ctx, ws); err != nil {
			return 0, err
		}
	}
	log.Info("sync cycle starting")

	sel, err := c.listings.Resolve(ctx, ws.Spec.Sources)
	if err != nil {
		log.Warn("sync cycle failed: listing", "error", err)
		return c.resync, c.fail(ctx, ws, trigger, v1alpha1.ReasonListingFailed, err.Error())
	}
	items := make([]provider.Item, len(sel.Items))
	for i, it := range sel.Items {
		items[i] = it.Item
	}
	manifest, err := workspace.ReadManifest(ctx, c.core, c.namespace, ws)
	if err != nil {
		return 0, err
	}
	changes, changed := changesSince(manifest, sel.Items)
	// A volume claimed anew is empty: whatever the manifest says, every
	// repository is cloned again.
	changed = changed || claim == nil

	provisioned, err := c.volumes.Provisioned(ctx)
	if errors.Is(err, ErrNoStorageClass) {
		log.Warn("sync cycle failed: no volume", "reason", v1alpha1.ReasonStorageClassMissing)
		return c.resync, c.fail(ctx, ws, trigger, v1alpha1.ReasonVolumeMissing, err.Error(), volumeClaimed(ws, metav1.ConditionFalse, v1alpha1.ReasonStorageClassMissing, err.Error()))
	}
	if err != nil {
		return 0, err
	}
	need := c.sizing.Need(manifest, items, ws.Status.Sessions)
	size := c.sizing.Size(need, ws.Spec.Sizing)
	if provisioned {
		if err := c.sizing.Exceeds(size); err != nil {
			log.Warn("sync cycle failed: size ceiling", "need", need, "size", size.String())
			return c.resync, c.fail(ctx, ws, trigger, v1alpha1.ReasonSizeCeiling, err.Error())
		}
	}
	request := NominalSize
	if provisioned {
		request = size
	}
	if claim, err = c.volumes.Claim(ctx, ws, request); err != nil {
		log.Warn("sync cycle failed: volume claim", "error", err)
		return c.resync, c.fail(ctx, ws, trigger, v1alpha1.ReasonVolumeMissing, err.Error(), volumeClaimed(ws, metav1.ConditionFalse, v1alpha1.ReasonVolumeClaimFailed, err.Error()))
	}
	claimed := volumeClaimed(ws, metav1.ConditionTrue, v1alpha1.ReasonVolumeClaimed, fmt.Sprintf("claim %s from StorageClass %s", claim.Name, c.volumes.StorageClass))
	var volumeSize *resource.Quantity
	if provisioned {
		// The share grows before the added repositories are mirrored.
		requested, err := c.volumes.Grow(ctx, claim, size)
		if err != nil {
			return 0, err
		}
		volumeSize = &requested
	}
	needed := *resource.NewQuantity(need, resource.BinarySI)

	cleanup, cleanedUp, err := c.cleanup(ctx, ws)
	if err != nil {
		return 0, err
	}
	cleanupOnly := !changed && housekeeping(ws, manifest, cleanup)
	if !changed && !cleanupOnly {
		now := c.now()
		log.Info("sync cycle done: no change", "repositories", len(items), "need", needed.String())
		return c.wakeAfter(ws, now), c.status.Update(ctx, ws.Name, func(w *v1alpha1.Workspace) bool {
			w.Status.ObservedGeneration = ws.Generation
			w.Status.LastSync = &v1alpha1.LastSync{Time: metav1.NewTime(now), Result: v1alpha1.SyncNoChange, Trigger: trigger}
			w.Status.NextSync = nextSync(w.Spec.Sync, now)
			w.Status.NeededSize, w.Status.VolumeSize = &needed, volumeSize
			w.Status.Manifest = ManifestName(ws)
			setVolume(w, claim)
			meta.SetStatusCondition(&w.Status.Conditions, claimed)
			meta.SetStatusCondition(&w.Status.Conditions, cleanedUp)
			meta.SetStatusCondition(&w.Status.Conditions, synced(ws, metav1.ConditionTrue, v1alpha1.ReasonNoChange, "the selection equals the last manifest and nothing was pushed since"))
			return true
		})
	}

	creds, err := c.credentials(ctx, sel)
	if err != nil {
		log.Warn("sync cycle failed: credentials", "error", err)
		return c.resync, c.fail(ctx, ws, trigger, v1alpha1.ReasonListingFailed, err.Error(), claimed)
	}
	id, err := cycleID()
	if err != nil {
		return 0, err
	}
	input := selectionOf(sel)
	input.Cleanup = cleanup
	job, err := c.jobs.Start(ctx, ws, id, input, creds)
	if err != nil {
		return 0, err
	}
	now := c.now()
	log.Info("sync job started", "job", job.Name, "added", changes.Added, "changed", changes.Changed, "removed", changes.Removed,
		"cleanupOnly", cleanupOnly, "need", needed.String(), "volumeSize", quantityString(volumeSize))
	return 0, c.status.Update(ctx, ws.Name, func(w *v1alpha1.Workspace) bool {
		w.Status.ObservedGeneration = ws.Generation
		w.Status.ActiveSync = &v1alpha1.ActiveSync{Job: job.Name, StartedAt: metav1.NewTime(now), Trigger: trigger, CleanupOnly: cleanupOnly}
		w.Status.NextSync = nil
		w.Status.NeededSize, w.Status.VolumeSize = &needed, volumeSize
		w.Status.Manifest = ManifestName(ws)
		setVolume(w, claim)
		meta.SetStatusCondition(&w.Status.Conditions, claimed)
		meta.SetStatusCondition(&w.Status.Conditions, cleanedUp)
		meta.SetStatusCondition(&w.Status.Conditions, synced(ws, metav1.ConditionUnknown, v1alpha1.ReasonSyncing, "sync job "+job.Name+" runs"))
		return true
	})
}

// finish records the active sync Job's outcome once it ended: the manifest,
// the repositories, the session directories, the measured need, the grown
// volume. It reports whether the cycle is over; while the Job runs, its
// completion wakes the controller.
func (c *Controller) finish(ctx context.Context, ws *v1alpha1.Workspace) (bool, error) {
	active := ws.Status.ActiveSync
	log := c.log.With("workspace", ws.Name, "job", active.Job, "trigger", active.Trigger)
	job, err := c.jobs.Get(ctx, ws, active.Job)
	if err != nil {
		return false, err
	}
	var cond *batchv1.JobCondition
	if job != nil {
		if cond = Finished(job); cond == nil {
			return false, nil
		}
	}
	defer func() {
		if err := c.jobs.ClearCredentials(ctx, ws); err != nil {
			log.Warn("credentials not cleared", "error", err)
		}
	}()

	ended := c.now()
	if cond != nil && !cond.LastTransitionTime.IsZero() {
		ended = cond.LastTransitionTime.Time
	}
	last := v1alpha1.LastSync{Time: metav1.NewTime(ended), Trigger: active.Trigger, Job: active.Job}
	var manifest *mirror.Manifest
	switch {
	case cond != nil && cond.Type == batchv1.JobFailed:
		last.Result, last.Message = v1alpha1.SyncFailed, c.syncError(ctx, ws, cond)
	default:
		if manifest, err = workspace.ReadManifest(ctx, c.core, c.namespace, ws); err != nil {
			return false, err
		}
		switch {
		case manifest != nil && (job != nil || manifest.SyncedAt.After(active.StartedAt.Time)):
			// A Job gone before its end was seen (its TTL, a restart) still
			// left its manifest, which is the cycle's result.
			last.Result = v1alpha1.SyncSucceeded
			if active.CleanupOnly {
				last.Result = v1alpha1.SyncNoChange
			}
			if job == nil {
				last.Message = "sync job " + active.Job + " is gone; its manifest stands"
			}
		case job == nil:
			last.Result, last.Message = v1alpha1.SyncFailed, "sync job "+active.Job+" is gone without a manifest"
		default:
			last.Result, last.Message = v1alpha1.SyncFailed, "sync job "+active.Job+" wrote no manifest"
		}
	}

	var needed, volumeSize *resource.Quantity
	var repositories []string
	var sessions []v1alpha1.SessionDirectory
	succeeded := last.Result != v1alpha1.SyncFailed
	if succeeded {
		repositories = repositoriesOf(manifest)
		sessions = sessionDirectories(manifest)
		need := c.sizing.Need(manifest, nil, sessions)
		needed = resource.NewQuantity(need, resource.BinarySI)
		var ceiling string
		if volumeSize, ceiling, err = c.growAfter(ctx, ws, need); err != nil {
			return false, err
		}
		if ceiling != "" {
			last.Message = ceiling
		}
		log.Info("sync cycle done", "repositories", len(repositories), "sessionDirectories", len(sessions), "cleanupOnly", active.CleanupOnly,
			"need", needed.String(), "volumeSize", quantityString(volumeSize), "ceiling", ceiling)
	} else {
		log.Warn("sync cycle failed", "error", last.Message)
	}
	return true, c.status.Update(ctx, ws.Name, func(w *v1alpha1.Workspace) bool {
		w.Status.ActiveSync = nil
		w.Status.LastSync = &last
		w.Status.NextSync = nextSync(w.Spec.Sync, ended)
		if w.DeletionTimestamp != nil {
			w.Status.NextSync = nil
		}
		if succeeded {
			w.Status.Repositories = repositories
			w.Status.Sessions = sessions
			w.Status.NeededSize = needed
			if volumeSize != nil {
				w.Status.VolumeSize = volumeSize
			}
			switch {
			case active.Trigger == v1alpha1.TriggerDeletion:
				meta.SetStatusCondition(&w.Status.Conditions, synced(ws, metav1.ConditionFalse, v1alpha1.ReasonDeleting,
					fmt.Sprintf("the workspace is deleted: its sync stopped, and %d session directories are left on its volume", len(sessions))))
			case active.CleanupOnly:
				meta.SetStatusCondition(&w.Status.Conditions, synced(ws, metav1.ConditionTrue, v1alpha1.ReasonNoChange,
					"the selection equals the last manifest and nothing was pushed since; sync job "+active.Job+" looked after the session directories"))
			default:
				meta.SetStatusCondition(&w.Status.Conditions, synced(ws, metav1.ConditionTrue, v1alpha1.ReasonSynced, "sync job "+active.Job+" succeeded"))
			}
		} else {
			w.Status.NextSync = &metav1.Time{Time: ended.Add(c.resync)}
			meta.SetStatusCondition(&w.Status.Conditions, synced(ws, metav1.ConditionFalse, v1alpha1.ReasonSyncFailed, last.Message))
		}
		return true
	})
}

// growAfter expands a provisioned share after a sync when its free space is
// below the headroom: to the formula's size, or to the ceiling when the
// formula passes it, which the message says. It returns the request in
// effect, nil on an elastic class.
func (c *Controller) growAfter(ctx context.Context, ws *v1alpha1.Workspace, need int64) (*resource.Quantity, string, error) {
	provisioned, err := c.volumes.Provisioned(ctx)
	if err != nil {
		return nil, "", err
	}
	if !provisioned {
		return nil, "", nil
	}
	claim, err := c.volumes.Get(ctx, ws)
	if err != nil || claim == nil {
		return nil, "", err
	}
	size := c.sizing.Size(need, ws.Spec.Sizing)
	var message string
	if err := c.sizing.Exceeds(size); err != nil {
		message = err.Error()
		size = *c.sizing.MaxSize
	}
	requested, err := c.volumes.Grow(ctx, claim, size)
	if err != nil {
		return nil, "", err
	}
	return &requested, message, nil
}

// fail records a cycle that ended before its Job: the result, the condition
// and the next try at the resync. Extra conditions are set with it.
func (c *Controller) fail(ctx context.Context, ws *v1alpha1.Workspace, trigger v1alpha1.SyncTrigger, reason, message string, conds ...metav1.Condition) error {
	now := c.now()
	return c.status.Update(ctx, ws.Name, func(w *v1alpha1.Workspace) bool {
		w.Status.ObservedGeneration = ws.Generation
		w.Status.LastSync = &v1alpha1.LastSync{Time: metav1.NewTime(now), Result: v1alpha1.SyncFailed, Trigger: trigger, Message: message}
		w.Status.NextSync = &metav1.Time{Time: now.Add(c.resync)}
		for _, cond := range conds {
			meta.SetStatusCondition(&w.Status.Conditions, cond)
		}
		meta.SetStatusCondition(&w.Status.Conditions, synced(ws, metav1.ConditionFalse, reason, message))
		return true
	})
}

// clearRequest removes the sync request annotations the cycle serves.
func (c *Controller) clearRequest(ctx context.Context, ws *v1alpha1.Workspace) error {
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:null,%q:null}}}`, v1alpha1.AnnotationSyncRequested, v1alpha1.AnnotationSyncRequestedBy)
	if _, err := c.dynamic.Resource(v1alpha1.WorkspaceResource).Namespace(ws.Namespace).Patch(ctx, ws.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("clear the sync request of workspace %s: %w", ws.Name, err)
	}
	return nil
}

// credentials are each provider instance's sync credential for each owner
// of the selection.
func (c *Controller) credentials(ctx context.Context, sel *workspace.Selection) ([]Credential, error) {
	var out []Credential
	seen := map[string]bool{}
	for _, it := range sel.Items {
		key := it.Provider + "/" + it.Owner
		if seen[key] {
			continue
		}
		seen[key] = true
		in, ok := c.instances[it.Provider]
		if !ok {
			return nil, fmt.Errorf("%w %q", workspace.ErrUnknownProvider, it.Provider)
		}
		src, err := in.SyncCredential(ctx, c.secrets, it.Owner)
		if err != nil {
			return nil, fmt.Errorf("the %s credential for %s: %w", it.Provider, it.Owner, err)
		}
		tok, err := src.Token()
		if err != nil {
			return nil, fmt.Errorf("the %s credential for %s: %w", it.Provider, it.Owner, err)
		}
		username := "x-access-token"
		if hosts := in.Hosts(); len(hosts.Git) > 0 && hosts.Git[0].User != "" {
			username = hosts.Git[0].User
		}
		out = append(out, Credential{Provider: it.Provider, Owner: it.Owner, Username: username, Token: tok.AccessToken})
	}
	return out, nil
}

// syncError is why a sync Job failed: what the sync wrote beside the
// manifest, else the Job's condition.
func (c *Controller) syncError(ctx context.Context, ws *v1alpha1.Workspace, cond *batchv1.JobCondition) string {
	if cm, err := c.core.CoreV1().ConfigMaps(c.namespace).Get(ctx, ManifestName(ws), metav1.GetOptions{}); err == nil {
		if msg := strings.TrimSpace(cm.Data[mirror.ErrorKey]); msg != "" {
			return msg
		}
	}
	if cond.Message != "" {
		return cond.Message
	}
	return "sync job failed"
}

func (c *Controller) wakeAfter(ws *v1alpha1.Workspace, now time.Time) time.Duration {
	next := workspace.NextSync(ws.Spec.Sync, now)
	if next.IsZero() {
		return 0
	}
	return max(next.Sub(now), time.Second)
}

// changesSince compares the selection with the manifest's live entries: a
// repository added, removed, pushed to since or whose default branch moved
// is a change.
func changesSince(manifest *mirror.Manifest, items []workspace.Selected) (provider.Changes, bool) {
	recorded := provider.State{}
	branches := map[string]string{}
	if manifest != nil {
		for _, e := range manifest.Repositories {
			if e.DroppedAt != nil {
				continue
			}
			recorded[e.Key()] = e.PushedAt
			branches[e.Key()] = e.DefaultBranch
		}
	}
	plain := make([]provider.Item, len(items))
	for i, it := range items {
		plain[i] = it.Item
	}
	changes := provider.Diff(recorded, plain)
	for _, it := range items {
		if b, ok := branches[it.Key()]; ok && b != it.DefaultBranch && !contains(changes.Changed, it.Key()) {
			changes.Changed = append(changes.Changed, it.Key())
		}
	}
	sort.Strings(changes.Changed)
	return changes, !changes.None()
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// selectionOf is the sync Job's input for the selection.
func selectionOf(sel *workspace.Selection) *mirror.Selection {
	out := &mirror.Selection{Repositories: make([]mirror.Repository, len(sel.Items))}
	for i, it := range sel.Items {
		out.Repositories[i] = mirror.Repository{Provider: it.Provider, Owner: it.Owner, Name: it.Name,
			CloneURL: it.CloneURL, DefaultBranch: it.DefaultBranch, PushedAt: it.LastChange.UTC()}
	}
	return out
}

// repositoriesOf are the manifest's live entries' keys, sorted.
func repositoriesOf(m *mirror.Manifest) []string {
	out := []string{}
	for _, e := range m.Repositories {
		if e.DroppedAt == nil {
			out = append(out, e.Key())
		}
	}
	sort.Strings(out)
	return out
}

func nextSync(s v1alpha1.Sync, after time.Time) *metav1.Time {
	next := workspace.NextSync(s, after)
	if next.IsZero() {
		return nil
	}
	return &metav1.Time{Time: next}
}

func synced(ws *v1alpha1.Workspace, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: v1alpha1.ConditionSynced, Status: status, Reason: reason, Message: message, ObservedGeneration: ws.Generation}
}

func volumeClaimed(ws *v1alpha1.Workspace, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: v1alpha1.ConditionVolumeClaimed, Status: status, Reason: reason, Message: message, ObservedGeneration: ws.Generation}
}

func setVolume(w *v1alpha1.Workspace, claim *corev1.PersistentVolumeClaim) {
	if claim == nil {
		return
	}
	if w.Status.Volume == nil {
		w.Status.Volume = &v1alpha1.VolumeRef{}
	}
	w.Status.Volume.ClaimName = claim.Name
}

func quantityString(q *resource.Quantity) string {
	if q == nil {
		return "none"
	}
	return q.String()
}

func cycleID() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("sync cycle id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
