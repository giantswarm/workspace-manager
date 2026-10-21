package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

// Sizing is a workspace's own sizing overrides, as Kubernetes quantities
// ("8Gi").
type Sizing struct {
	Headroom string `json:"headroom,omitempty" jsonschema:"Replaces the computed headroom when larger, a Kubernetes quantity such as 8Gi"`
	Minimum  string `json:"minimum,omitempty" jsonschema:"The smallest size the volume gets, a Kubernetes quantity such as 32Gi"`
}

// CreateInput is create_workspace's input.
type CreateInput struct {
	Name         string            `json:"name" jsonschema:"The workspace's name: lower-case letters, digits and '-', unique in the installation"`
	Organization string            `json:"organization" jsonschema:"The Organization the workspace belongs to, for good; you must be a member"`
	Sources      []v1alpha1.Source `json:"sources" jsonschema:"One owner on one provider instance each, with at least one selector: repositories by name, filters.languages or filters.topics"`
	Sizing       *Sizing           `json:"sizing,omitempty" jsonschema:"Overrides of the volume's computed size where its StorageClass provisions one"`
}

// UpdateInput is update_workspace's input.
type UpdateInput struct {
	Name    string            `json:"name" jsonschema:"The workspace's name"`
	Sources []v1alpha1.Source `json:"sources" jsonschema:"The workspace's sources, replacing the current ones"`
	Sizing  *Sizing           `json:"sizing,omitempty" jsonschema:"Replaces the sizing overrides when given ({} clears them); kept otherwise"`
	// ResourceVersion, when given, refuses the update if the workspace
	// changed since it was read.
	ResourceVersion string `json:"resourceVersion,omitempty" jsonschema:"The resourceVersion get_workspace returned; the update is refused when the workspace changed since"`
}

// Workspace is a workspace as the tools return it.
type Workspace struct {
	Name            string            `json:"name"`
	Organization    string            `json:"organization"`
	Sources         []v1alpha1.Source `json:"sources"`
	Sizing          *Sizing           `json:"sizing,omitempty"`
	ResourceVersion string            `json:"resourceVersion"`
	CreatedAt       time.Time         `json:"createdAt"`
	// Deleting is true once the workspace is being deleted: no new Session
	// starts on it.
	Deleting bool `json:"deleting,omitempty"`
	// LastSync is the last sync cycle's time and result; ActiveSync the sync
	// Job running now; SyncRequest a sync request (sync_workspace, or
	// authorize_workspace past the refresh threshold) the controller has not
	// taken up yet.
	LastSync    *LastSync    `json:"lastSync,omitempty"`
	ActiveSync  *ActiveSync  `json:"activeSync,omitempty"`
	SyncRequest *SyncRequest `json:"syncRequest,omitempty"`
	// Repositories is the number of repositories the last sync resolved.
	Repositories int `json:"repositories"`
	// Volume is the workspace's volume, by its claim and CSI handle once
	// bound: the handle a Session's volume reference names.
	Volume *Volume `json:"volume,omitempty"`
	// NeededSize and VolumeSize are what the volume needs and has, where
	// its StorageClass provisions a size.
	NeededSize string `json:"neededSize,omitempty"`
	VolumeSize string `json:"volumeSize,omitempty"`
	// StoredBytes is what the volume stores, measured at the last sync: the
	// mirrors and the Session directories, which get_workspace's storage
	// splits; absent before the first sync.
	StoredBytes *int64 `json:"storedBytes,omitempty"`
	// GrewFrom is the provisioned share's size before the last sync grew it;
	// empty when the last sync did not grow it.
	GrewFrom string `json:"grewFrom,omitempty"`
	// Sessions is the number of Session directories on the volume.
	Sessions   int         `json:"sessions"`
	Conditions []Condition `json:"conditions,omitempty"`
}

// LastSync is a sync cycle's outcome.
type LastSync struct {
	Time   time.Time `json:"time"`
	Result string    `json:"result"`
	// Trigger is what started the cycle: change, request, retry, cleanup,
	// deletion or sessionDeleted.
	Trigger string `json:"trigger,omitempty"`
	// Job is the sync Job the cycle ran; empty for a cycle that ran none.
	Job string `json:"job,omitempty"`
	// RequestIDs are the sync_workspace requests the cycle served, by the
	// requestId it returned, ascending.
	RequestIDs []string `json:"requestIds,omitempty"`
	Message    string   `json:"message,omitempty"`
}

// ActiveSync is the sync Job running now.
type ActiveSync struct {
	Job       string    `json:"job"`
	StartedAt time.Time `json:"startedAt"`
	Trigger   string    `json:"trigger,omitempty"`
	// RequestIDs are the sync_workspace requests the cycle serves.
	RequestIDs []string `json:"requestIds,omitempty"`
}

// SyncRequest is a sync requested through sync_workspace, until the
// controller starts its cycle.
type SyncRequest struct {
	RequestedAt time.Time `json:"requestedAt"`
	RequestedBy string    `json:"requestedBy,omitempty"`
	// RequestIDs are the requests pending, ascending; the next cycle serves
	// them all.
	RequestIDs []string `json:"requestIds,omitempty"`
}

// SyncResult is sync_workspace's result.
type SyncResult struct {
	Workspace string `json:"workspace"`
	// RequestID identifies the request: the cycle that serves it names it in
	// get_workspace's lastSync.requestIds (activeSync.requestIds while it
	// runs, syncRequest.requestIds until it starts). A workspace's
	// identifiers ascend in the order of their requests, so one that is in
	// neither syncRequest nor activeSync was served, by lastSync's cycle or
	// an earlier one.
	RequestID   string    `json:"requestId"`
	RequestedAt time.Time `json:"requestedAt"`
	RequestedBy string    `json:"requestedBy,omitempty"`
	// ActiveSync is the sync Job running at the request, after which the
	// requested cycle starts.
	ActiveSync *ActiveSync `json:"activeSync,omitempty"`
	// Deleting is true on a workspace being deleted: the request runs its
	// deletion cycle, which only cleans up the session directories.
	Deleting bool `json:"deleting,omitempty"`
}

// Volume is the workspace's volume.
type Volume struct {
	ClaimName string `json:"claimName,omitempty"`
	Driver    string `json:"driver,omitempty"`
	Handle    string `json:"handle,omitempty"`
	// Pending says why there is no CSI handle yet.
	Pending string `json:"pending,omitempty"`
}

// Condition is one of the workspace's conditions.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// WorkspacesResult is list_workspaces' result.
type WorkspacesResult struct {
	Workspaces []Workspace `json:"workspaces"`
	// Organizations are every Organization the caller is a member of, with
	// or without a workspace and whatever the organization filter: where
	// the caller may create one.
	Organizations []string `json:"organizations"`
}

// WorkspaceDetail is get_workspace's result.
type WorkspaceDetail struct {
	Workspace Workspace `json:"workspace"`
	// Mirrors are the last sync's repositories with their measured mirror
	// sizes; empty before the first sync.
	Mirrors []Mirror `json:"mirrors"`
	// Storage is what the volume stores.
	Storage Storage `json:"storage"`
	// SessionDirectories are the Sessions with a directory on the volume.
	SessionDirectories []SessionDirectory `json:"sessionDirectories"`
}

// Mirror is one repository on the volume.
type Mirror struct {
	Provider      string    `json:"provider"`
	Repository    string    `json:"repository"`
	DefaultBranch string    `json:"defaultBranch,omitempty"`
	Head          string    `json:"head,omitempty"`
	PushedAt      time.Time `json:"pushedAt"`
	SizeBytes     int64     `json:"sizeBytes"`
	// DroppedAt is set for a repository that left the selection, whose
	// mirror stays while a Session's directory borrows from it.
	DroppedAt *time.Time `json:"droppedAt,omitempty"`
}

// Storage is what the volume stores: the mirrors and the Session
// directories, measured at the last sync, and the share's size where the
// StorageClass provisions one.
type Storage struct {
	MirrorsBytes  int64  `json:"mirrorsBytes"`
	SessionsBytes int64  `json:"sessionsBytes"`
	NeededSize    string `json:"neededSize,omitempty"`
	VolumeSize    string `json:"volumeSize,omitempty"`
}

// SessionDirectory is one Session's directory on the volume.
type SessionDirectory struct {
	// Session is the Session's id; Name, Creator and LastTurn are kagent's.
	Session  string     `json:"session"`
	Name     string     `json:"name,omitempty"`
	Creator  string     `json:"creator,omitempty"`
	LastTurn *time.Time `json:"lastTurn,omitempty"`
	// Unknown marks a directory whose Session kagent does not list: it goes
	// after the cleanup window from when a sync first found it.
	Unknown   bool  `json:"unknown,omitempty"`
	SizeBytes int64 `json:"sizeBytes"`
	// CleanupAfter is when the directory is deleted.
	CleanupAfter *time.Time `json:"cleanupAfter,omitempty"`
	// Error is why the last sync could neither measure nor delete it.
	Error string `json:"error,omitempty"`
}

// DeleteResult is delete_workspace's result.
type DeleteResult struct {
	Workspace string `json:"workspace"`
	// Sessions are the Sessions with a directory on the volume at the
	// delete.
	Sessions []string `json:"sessions"`
	// AlreadyDeleting is true when the workspace was being deleted before
	// the call: the call asked for its deletion cycle now instead.
	AlreadyDeleting bool `json:"alreadyDeleting,omitempty"`
}

// workspaceTools serve the Workspace tools: every read and write goes
// through the Store, which checks the caller's Organization and writes with
// the manager's ServiceAccount.
type workspaceTools struct {
	store     *workspace.Store
	core      kubernetes.Interface
	namespace string
	now       func() time.Time
}

func addWorkspaceTools(s *mcpserver.MCPServer, t *workspaceTools) {
	nameArg := mcp.WithString("workspace", mcp.Required(), mcp.Description("The workspace's name"))

	s.AddTool(mcp.NewTool("list_workspaces",
		tooldesc.Describe("List the workspaces of your Organizations: sources, last sync, repository count, volume and Session count.",
			"volume carries its CSI handle; storedBytes is what the volume stored at the last sync, grewFrom the size a provisioned share grew from then. "+
				"organizations names every Organization you are a member of, with or without a workspace: where you may create one."),
		mcp.WithString("organization", mcp.Description("Only this Organization's workspaces; you must be a member")),
		readOnly(),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithOutputSchema[WorkspacesResult](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		org := req.GetString("organization", "")
		if org != "" {
			if err := t.store.CheckOrganization(ctx, org); err != nil {
				return toolError("list workspaces", err), nil
			}
		}
		list, err := t.store.List(ctx)
		if err != nil {
			return toolError("list workspaces", err), nil
		}
		out := WorkspacesResult{Workspaces: []Workspace{}, Organizations: t.store.Organizations(ctx)}
		for i := range list {
			if org != "" && list[i].Spec.Organization != org {
				continue
			}
			w, err := t.view(ctx, &list[i])
			if err != nil {
				return toolError("list workspaces", err), nil
			}
			out.Workspaces = append(out.Workspaces, w)
		}
		return mcp.NewToolResultStructured(out, fmt.Sprintf("%d workspaces; member of %d Organizations: %s",
			len(out.Workspaces), len(out.Organizations), strings.Join(out.Organizations, ", "))), nil
	})

	s.AddTool(mcp.NewTool("get_workspace",
		tooldesc.Describe("Get a workspace of your Organization: sources, last and running sync, mirrors, volume storage and Session directories.",
			"The mirrors are the last sync's repositories with their sizes measured then; each Session directory comes with its size and cleanup date."),
		nameArg,
		readOnly(),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithOutputSchema[WorkspaceDetail](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, err := req.RequireString("workspace")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		ws, err := t.store.Get(ctx, name)
		if err != nil {
			return toolError("get workspace", err), nil
		}
		d, err := t.detail(ctx, ws)
		if err != nil {
			return toolError("get workspace", err), nil
		}
		return mcp.NewToolResultStructured(*d, fmt.Sprintf("Workspace %s of %s: %d repositories, %d Session directories.",
			name, ws.Spec.Organization, len(d.Mirrors), len(d.SessionDirectories))), nil
	})

	s.AddTool(mcp.NewTool("create_workspace",
		tooldesc.Describe("Create a workspace of an Organization you are a member of; it syncs after the save.",
			"It syncs again when a Session starts on it and its last sync is older than the refresh threshold."),
		mcp.WithInputSchema[CreateInput](),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithOutputSchema[Workspace](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in CreateInput
		if err := req.BindArguments(&in); err != nil {
			return mcp.NewToolResultError("invalid arguments: " + err.Error()), nil
		}
		ws := &v1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: t.namespace},
			Spec:       v1alpha1.WorkspaceSpec{Organization: in.Organization, Sources: in.Sources},
		}
		var err error
		if ws.Spec.Sizing, err = in.Sizing.spec(); err != nil {
			return toolError("create workspace", err), nil
		}
		created, err := t.store.Create(ctx, ws)
		if err != nil {
			return toolError("create workspace", err), nil
		}
		slog.InfoContext(ctx, "workspace created", identity.LogAttr(ctx), "workspace", created.Name, "organization", created.Spec.Organization, "sources", len(created.Spec.Sources))
		w, err := t.view(ctx, created)
		if err != nil {
			return toolError("create workspace", err), nil
		}
		return mcp.NewToolResultStructured(w, fmt.Sprintf("Created workspace %s of %s.", created.Name, created.Spec.Organization)), nil
	})

	s.AddTool(mcp.NewTool("update_workspace",
		tooldesc.Describe("Change a workspace of your Organization: its sources, and its sizing when given.",
			"Its Organization never changes. It syncs after the save, and again when a Session starts on it and its last sync is older than the refresh threshold."),
		mcp.WithInputSchema[UpdateInput](),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithOutputSchema[Workspace](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in UpdateInput
		if err := req.BindArguments(&in); err != nil {
			return mcp.NewToolResultError("invalid arguments: " + err.Error()), nil
		}
		current, err := t.store.Get(ctx, in.Name)
		if err != nil {
			return toolError("update workspace", err), nil
		}
		next := current.DeepCopy()
		next.Spec.Sources = in.Sources
		if in.Sizing != nil {
			if next.Spec.Sizing, err = in.Sizing.spec(); err != nil {
				return toolError("update workspace", err), nil
			}
		}
		if in.ResourceVersion != "" {
			next.ResourceVersion = in.ResourceVersion
		}
		updated, err := t.store.Update(ctx, next)
		if err != nil {
			return toolError("update workspace", err), nil
		}
		slog.InfoContext(ctx, "workspace updated", identity.LogAttr(ctx), "workspace", updated.Name, "organization", updated.Spec.Organization, "sources", len(updated.Spec.Sources))
		w, err := t.view(ctx, updated)
		if err != nil {
			return toolError("update workspace", err), nil
		}
		return mcp.NewToolResultStructured(w, fmt.Sprintf("Updated workspace %s.", updated.Name)), nil
	})

	s.AddTool(mcp.NewTool("delete_workspace",
		tooldesc.Describe("Delete a workspace of your Organization: its sync stops and no new Session starts on it.",
			"Returns the Sessions that still have a directory on its volume. "+
				"On a workspace already being deleted, it runs the deletion cycle now: session directories found gone let the workspace and its volume go without waiting for the next cleanup date."),
		nameArg,
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithOutputSchema[DeleteResult](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, err := req.RequireString("workspace")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		ws, err := t.store.Delete(ctx, name, t.now())
		if err != nil {
			return toolError("delete workspace", err), nil
		}
		res := DeleteResult{Workspace: name, Sessions: []string{}, AlreadyDeleting: ws.DeletionTimestamp != nil}
		for _, s := range ws.Status.Sessions {
			res.Sessions = append(res.Sessions, s.Session)
		}
		text := fmt.Sprintf("Deleted workspace %s: its sync stops and no new Session starts on it.", name)
		if res.AlreadyDeleting {
			slog.InfoContext(ctx, "workspace deletion cycle requested", identity.LogAttr(ctx), "workspace", name, "organization", ws.Spec.Organization, "sessions", len(res.Sessions))
			text = fmt.Sprintf("Workspace %s is already being deleted: the controller runs its deletion cycle now, and the workspace goes once no session directory is left.", name)
		} else {
			slog.InfoContext(ctx, "workspace deleted", identity.LogAttr(ctx), "workspace", name, "organization", ws.Spec.Organization, "sessions", len(res.Sessions))
		}
		if len(res.Sessions) > 0 {
			text += fmt.Sprintf(" %d Sessions still have a directory on it: %s.", len(res.Sessions), strings.Join(res.Sessions, ", "))
		}
		return mcp.NewToolResultStructured(res, text), nil
	})

	s.AddTool(mcp.NewTool("sync_workspace",
		tooldesc.Describe("Request a sync of a workspace of your Organization now: the controller fetches what changed in its sources.",
			"It grows the volume where its StorageClass provisions a size. Nothing changed costs no fetch; a request made while a sync runs is served after it. "+
				"On a workspace being deleted it runs the deletion cycle now, which only cleans up the session directories. "+
				"It returns a requestId; get_workspace names it in syncRequest, then activeSync, then lastSync, the requestIds of the cycle that served it. "+
				"Identifiers ascend, so one in neither syncRequest nor activeSync was served."),
		nameArg,
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithOutputSchema[SyncResult](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, err := req.RequireString("workspace")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		ws, err := t.store.RequestSync(ctx, name, t.now())
		if err != nil {
			return toolError("sync workspace", err), nil
		}
		res := SyncResult{Workspace: name, RequestedBy: ws.Annotations[v1alpha1.AnnotationSyncRequestedBy], ActiveSync: activeSyncOf(ws.Status.ActiveSync),
			Deleting: ws.DeletionTimestamp != nil}
		if ids := v1alpha1.SyncRequestIDs(ws); len(ids) > 0 {
			res.RequestID = ids[len(ids)-1]
		}
		res.RequestedAt, _ = time.Parse(time.RFC3339, ws.Annotations[v1alpha1.AnnotationSyncRequested])
		slog.InfoContext(ctx, "workspace sync requested", identity.LogAttr(ctx), "workspace", name, "organization", ws.Spec.Organization, "requestId", res.RequestID)
		text := fmt.Sprintf("Sync of workspace %s requested: the controller starts its cycle now.", name)
		if res.Deleting {
			text = fmt.Sprintf("Deletion cycle of workspace %s requested: the workspace is being deleted, and the controller runs its deletion cycle now.", name)
		}
		if res.ActiveSync != nil {
			text = fmt.Sprintf("Sync of workspace %s requested: sync job %s runs, and the requested cycle starts after it.", name, res.ActiveSync.Job)
		}
		return mcp.NewToolResultStructured(res, text), nil
	})
}

// view is a workspace as the tools return it, its volume's CSI handle read
// from the bound PersistentVolume.
func (t *workspaceTools) view(ctx context.Context, ws *v1alpha1.Workspace) (Workspace, error) {
	w := Workspace{
		Name:            ws.Name,
		Organization:    ws.Spec.Organization,
		Sources:         ws.Spec.Sources,
		Sizing:          sizingOf(ws.Spec.Sizing),
		ResourceVersion: ws.ResourceVersion,
		CreatedAt:       ws.CreationTimestamp.UTC(),
		Deleting:        ws.DeletionTimestamp != nil,
		Repositories:    len(ws.Status.Repositories),
		NeededSize:      quantity(ws.Status.NeededSize),
		VolumeSize:      quantity(ws.Status.VolumeSize),
		GrewFrom:        quantity(ws.Status.GrewFrom),
		Sessions:        len(ws.Status.Sessions),
	}
	if w.Sources == nil {
		w.Sources = []v1alpha1.Source{}
	}
	if s := ws.Status.StoredSize; s != nil {
		stored := s.Value()
		w.StoredBytes = &stored
	}
	if ls := ws.Status.LastSync; ls != nil {
		w.LastSync = &LastSync{Time: ls.Time.UTC(), Result: string(ls.Result), Trigger: string(ls.Trigger), Job: ls.Job, RequestIDs: ls.Requests, Message: ls.Message}
	}
	w.ActiveSync = activeSyncOf(ws.Status.ActiveSync)
	if at, err := time.Parse(time.RFC3339, ws.Annotations[v1alpha1.AnnotationSyncRequested]); err == nil {
		w.SyncRequest = &SyncRequest{RequestedAt: at.UTC(), RequestedBy: ws.Annotations[v1alpha1.AnnotationSyncRequestedBy], RequestIDs: v1alpha1.SyncRequestIDs(ws)}
	}
	for _, c := range ws.Status.Conditions {
		w.Conditions = append(w.Conditions, Condition{Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message})
	}
	if ws.Status.Volume != nil {
		ref, reason, err := workspace.Volume(ctx, t.core, t.namespace, ws)
		if err != nil {
			return w, err
		}
		w.Volume = &Volume{ClaimName: ws.Status.Volume.ClaimName, Pending: reason}
		if ref != nil {
			w.Volume.Driver, w.Volume.Handle = ref.Driver, ref.Handle
		}
	}
	return w, nil
}

// detail is get_workspace's answer: the view, the last sync's manifest and
// the Session directories.
func (t *workspaceTools) detail(ctx context.Context, ws *v1alpha1.Workspace) (*WorkspaceDetail, error) {
	w, err := t.view(ctx, ws)
	if err != nil {
		return nil, err
	}
	d := &WorkspaceDetail{Workspace: w, Mirrors: []Mirror{}, SessionDirectories: []SessionDirectory{},
		Storage: Storage{NeededSize: w.NeededSize, VolumeSize: w.VolumeSize}}
	manifest, err := workspace.ReadManifest(ctx, t.core, t.namespace, ws)
	if err != nil {
		return nil, err
	}
	if manifest != nil {
		for _, e := range manifest.Repositories {
			m := Mirror{Provider: e.Provider, Repository: e.Key(), DefaultBranch: e.DefaultBranch, Head: e.Head,
				PushedAt: e.PushedAt.UTC(), SizeBytes: e.SizeBytes, DroppedAt: e.DroppedAt}
			d.Mirrors = append(d.Mirrors, m)
			d.Storage.MirrorsBytes += e.SizeBytes
		}
	}
	for _, s := range ws.Status.Sessions {
		sd := SessionDirectory{Session: s.Session, Name: s.Name, Creator: s.Creator, Unknown: s.Unknown, Error: s.Error}
		if s.LastTurn != nil {
			at := s.LastTurn.UTC()
			sd.LastTurn = &at
		}
		if s.Size != nil {
			sd.SizeBytes = s.Size.Value()
		}
		if s.CleanupAfter != nil {
			at := s.CleanupAfter.UTC()
			sd.CleanupAfter = &at
		}
		d.SessionDirectories = append(d.SessionDirectories, sd)
		d.Storage.SessionsBytes += sd.SizeBytes
	}
	return d, nil
}

// spec is the overrides as the Workspace's spec holds them; nil for none.
func (s *Sizing) spec() (*v1alpha1.Sizing, error) {
	if s == nil {
		return nil, nil
	}
	out := &v1alpha1.Sizing{}
	for field, v := range map[string]struct {
		in  string
		out **resource.Quantity
	}{"headroom": {s.Headroom, &out.Headroom}, "minimum": {s.Minimum, &out.Minimum}} {
		if v.in == "" {
			continue
		}
		q, err := resource.ParseQuantity(v.in)
		if err != nil {
			return nil, fmt.Errorf("%w: sizing.%s %q: %w", workspace.ErrInvalid, field, v.in, err)
		}
		*v.out = &q
	}
	if out.Headroom == nil && out.Minimum == nil {
		return nil, nil
	}
	return out, nil
}

func activeSyncOf(a *v1alpha1.ActiveSync) *ActiveSync {
	if a == nil {
		return nil
	}
	return &ActiveSync{Job: a.Job, StartedAt: a.StartedAt.UTC(), Trigger: string(a.Trigger), RequestIDs: a.Requests}
}

func sizingOf(s *v1alpha1.Sizing) *Sizing {
	if s == nil || (s.Headroom == nil && s.Minimum == nil) {
		return nil
	}
	return &Sizing{Headroom: quantity(s.Headroom), Minimum: quantity(s.Minimum)}
}

func quantity(q *resource.Quantity) string {
	if q == nil {
		return ""
	}
	return q.String()
}

// toolError is a failed call's answer: a refusal says what was refused and
// why, so a non-member gets a permission error and an invalid spec its
// validation message, never a server error.
func toolError(op string, err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, workspace.ErrForbidden):
		return mcp.NewToolResultError(op + ": permission denied: " + err.Error())
	case errors.Is(err, workspace.ErrInvalid), errors.Is(err, workspace.ErrUnknownProvider), apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return mcp.NewToolResultError(op + ": invalid workspace: " + message(err))
	case apierrors.IsNotFound(err):
		return mcp.NewToolResultError(op + ": not found: " + message(err))
	case apierrors.IsAlreadyExists(err):
		return mcp.NewToolResultError(op + ": already exists: " + message(err))
	case apierrors.IsConflict(err):
		return mcp.NewToolResultError(op + ": the workspace changed since it was read; read it again and retry: " + message(err))
	}
	return mcp.NewToolResultErrorFromErr(op, err)
}

// message is the API server's own message for an API error, which names
// what it refused; the error's text otherwise.
func message(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) && !errors.Is(err, workspace.ErrInvalid) {
		return status.Status().Message
	}
	return err.Error()
}
