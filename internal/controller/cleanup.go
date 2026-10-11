package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/kagent"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

// Sessions lists kagent's Sessions, every creator's, to the end
// (kagent.Client).
type Sessions interface {
	Sessions(ctx context.Context) ([]kagent.Session, error)
}

// cleanup is the session directories' cleanup a cycle hands its Job, with
// the condition saying how it was made. It is nil when no directory may be
// deleted this cycle: the manager runs without kagent, kagent's listing
// failed, or the volume's CSI handle, which the Sessions name it by, is not
// known yet; the Job then only measures the directories.
func (c *Controller) cleanup(ctx context.Context, ws *v1alpha1.Workspace) (*mirror.Cleanup, metav1.Condition, error) {
	if c.sessions == nil {
		return nil, sessionsCleanedUp(ws, metav1.ConditionFalse, v1alpha1.ReasonKagentNotConfigured,
			"no session directory is cleaned up: the manager runs without kagent, whose Sessions' last turns the cleanup works from"), nil
	}
	ref, reason, err := workspace.Volume(ctx, c.core, c.namespace, ws)
	if err != nil {
		return nil, metav1.Condition{}, err
	}
	if ref == nil {
		return nil, sessionsCleanedUp(ws, metav1.ConditionFalse, v1alpha1.ReasonVolumePending, "no session directory is cleaned up: "+reason), nil
	}
	sessions, err := c.sessions.Sessions(ctx)
	if err != nil {
		c.log.Warn("session directories not cleaned up: kagent's listing failed", "workspace", ws.Name, "error", err)
		return nil, sessionsCleanedUp(ws, metav1.ConditionFalse, v1alpha1.ReasonKagentUnavailable, "no session directory is cleaned up: "+err.Error()), nil
	}
	out := &mirror.Cleanup{AfterSeconds: int64(c.sessionCleanupAfter / time.Second), Sessions: map[string]mirror.Session{}}
	for _, s := range sessions {
		if s.Driver != ref.Driver || s.Handle != ref.Handle {
			continue
		}
		out.Sessions[s.ID] = mirror.Session{Name: s.Name, Creator: s.Creator, CreatedAt: s.CreatedAt.UTC(), LastTurn: s.LastTurn.UTC()}
	}
	return out, sessionsCleanedUp(ws, metav1.ConditionTrue, v1alpha1.ReasonSessionsListed,
		fmt.Sprintf("kagent lists %d Sessions on the volume; their directories go %s after the last turn", len(out.Sessions), c.sessionCleanupAfter)), nil
}

// housekeeping reports whether a cycle that fetches nothing still runs its
// Job: session directories to clean up or measure, or a dropped mirror that
// may be deleted.
func housekeeping(ws *v1alpha1.Workspace, manifest *mirror.Manifest, cleanup *mirror.Cleanup) bool {
	if len(ws.Status.Sessions) > 0 || (cleanup != nil && len(cleanup.Sessions) > 0) {
		return true
	}
	if manifest != nil {
		if len(manifest.Sessions) > 0 {
			return true
		}
		for _, e := range manifest.Repositories {
			if e.DroppedAt != nil {
				return true
			}
		}
	}
	return false
}

// sessionDirectories is the status's inventory from a manifest.
func sessionDirectories(m *mirror.Manifest) []v1alpha1.SessionDirectory {
	if m == nil || len(m.Sessions) == 0 {
		return nil
	}
	out := make([]v1alpha1.SessionDirectory, 0, len(m.Sessions))
	for _, d := range m.Sessions {
		sd := v1alpha1.SessionDirectory{Session: d.Session, Name: d.Name, Creator: d.Creator, Unknown: !d.Known,
			Size: resource.NewQuantity(d.SizeBytes, resource.BinarySI)}
		if d.LastTurn != nil {
			sd.LastTurn = &metav1.Time{Time: *d.LastTurn}
		}
		if !d.CleanupAfter.IsZero() {
			sd.CleanupAfter = &metav1.Time{Time: d.CleanupAfter}
		}
		out = append(out, sd)
	}
	return out
}

// reconcileDeleted runs a deleted workspace to its end: its sync stopped, its
// Job only cleans up the session directories, at the workspace's schedule
// or at the earliest cleanup date, whichever comes first (a failed one again
// after the resync); once a cycle since the deletion leaves no directory,
// the volume claim is deleted and the finalizer removed, and everything else
// the workspace owns goes with it.
func (c *Controller) reconcileDeleted(ctx context.Context, ws *v1alpha1.Workspace) (time.Duration, error) {
	if !slices.Contains(ws.Finalizers, v1alpha1.FinalizerVolume) {
		return 0, nil
	}
	log := c.log.With("workspace", ws.Name, "trigger", v1alpha1.TriggerDeletion)
	if ws.Status.ActiveSync != nil {
		done, err := c.finish(ctx, ws)
		if err != nil || !done {
			return 0, err
		}
		if ws, err = c.get(ctx, ws); err != nil {
			return 0, err
		}
	}
	claim, err := c.volumes.Get(ctx, ws)
	if err != nil {
		return 0, err
	}
	last := ws.Status.LastSync
	cleaned := last != nil && last.Trigger == v1alpha1.TriggerDeletion && last.Result != v1alpha1.SyncFailed
	if claim == nil || (cleaned && len(ws.Status.Sessions) == 0) {
		if claim != nil {
			if err := c.volumes.Delete(ctx, claim); err != nil {
				return 0, err
			}
			log.Info("workspace volume deleted: its last session directory is gone", "claim", claim.Name)
		}
		return 0, c.removeFinalizer(ctx, ws)
	}
	now := c.now()
	if last != nil && last.Trigger == v1alpha1.TriggerDeletion {
		at := last.Time.Add(c.resync)
		if cleaned {
			at = c.nextDeletionCycle(ws, last.Time.Time)
		}
		if at.After(now) {
			return at.Sub(now), nil
		}
	}

	log.Info("deletion cycle starting", "sessionDirectories", len(ws.Status.Sessions))
	cleanup, cond, err := c.cleanup(ctx, ws)
	if err != nil {
		return 0, err
	}
	id, err := cycleID()
	if err != nil {
		return 0, err
	}
	job, err := c.jobs.Start(ctx, ws, id, &mirror.Selection{Repositories: []mirror.Repository{}, Stopped: true, Cleanup: cleanup}, nil)
	if err != nil {
		return 0, err
	}
	log.Info("sync job started for the session directories", "job", job.Name)
	return 0, c.status.Update(ctx, ws.Name, func(w *v1alpha1.Workspace) bool {
		w.Status.ActiveSync = &v1alpha1.ActiveSync{Job: job.Name, StartedAt: metav1.NewTime(now), Trigger: v1alpha1.TriggerDeletion, CleanupOnly: true}
		w.Status.NextSync = nil
		meta.SetStatusCondition(&w.Status.Conditions, cond)
		meta.SetStatusCondition(&w.Status.Conditions, synced(ws, metav1.ConditionFalse, v1alpha1.ReasonDeleting,
			fmt.Sprintf("the workspace is deleted: its sync stopped, and sync job %s cleans up its %d session directories", job.Name, len(ws.Status.Sessions))))
		return true
	})
}

// nextDeletionCycle is when a deleted workspace's next cleanup runs: at its
// schedule after the last one, or at the earliest cleanup date when that
// comes first; the resync when the schedule names no time.
func (c *Controller) nextDeletionCycle(ws *v1alpha1.Workspace, last time.Time) time.Time {
	at := workspace.NextSync(ws.Spec.Sync, last)
	if at.IsZero() {
		at = last.Add(c.resync)
	}
	for _, d := range ws.Status.Sessions {
		if d.CleanupAfter != nil && d.CleanupAfter.Before(&metav1.Time{Time: at}) {
			at = d.CleanupAfter.Time
		}
	}
	return at
}

// ensureFinalizer puts the volume finalizer on a live workspace, so its
// deletion waits for its session directories.
func (c *Controller) ensureFinalizer(ctx context.Context, ws *v1alpha1.Workspace) error {
	if slices.Contains(ws.Finalizers, v1alpha1.FinalizerVolume) {
		return nil
	}
	return c.patchFinalizers(ctx, ws, append(slices.Clone(ws.Finalizers), v1alpha1.FinalizerVolume))
}

// removeFinalizer lets a deleted workspace go.
func (c *Controller) removeFinalizer(ctx context.Context, ws *v1alpha1.Workspace) error {
	kept := slices.DeleteFunc(slices.Clone(ws.Finalizers), func(f string) bool { return f == v1alpha1.FinalizerVolume })
	return c.patchFinalizers(ctx, ws, kept)
}

// patchFinalizers replaces the workspace's finalizers, refused when the
// workspace changed since it was read.
func (c *Controller) patchFinalizers(ctx context.Context, ws *v1alpha1.Workspace, finalizers []string) error {
	if finalizers == nil {
		finalizers = []string{}
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": finalizers, "resourceVersion": ws.ResourceVersion}})
	if err != nil {
		return err
	}
	u, err := c.dynamic.Resource(v1alpha1.WorkspaceResource).Namespace(ws.Namespace).Patch(ctx, ws.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("set the finalizers of workspace %s: %w", ws.Name, err)
	}
	ws.Finalizers, ws.ResourceVersion = u.GetFinalizers(), u.GetResourceVersion()
	return nil
}

func sessionsCleanedUp(ws *v1alpha1.Workspace, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: v1alpha1.ConditionSessionsCleanedUp, Status: status, Reason: reason, Message: message, ObservedGeneration: ws.Generation}
}
