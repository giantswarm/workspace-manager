// Package authorize is the access check at a Session's start: may the person
// read every repository of the workspace's last sync? With the person's own
// sign-in to each provider of the workspace, one check per owner tells which
// of the manifest's repositories they read. All of them: a signed grant
// (package grant) naming the person, the workspace, its volume and the
// repositories pushed to since the sync. Otherwise a refusal naming the
// repositories, or, for a provider the person has not connected, its connect
// link. It runs before the workspace's volume is mounted anywhere, because
// the volume's content is readable in the Session from its first turn. A
// granted check on a workspace whose last sync is older than the refresh
// threshold also requests a sync, which the Session does not wait for: the
// mirrors are refreshed at Session start, never on a schedule.
//
// The person has two names here. Their sign-ins are keyed by the Dex subject
// (package signin). The grant names them by email: kagent's Session admission
// compares the grant's `sub` with the Session's creator as kagent's
// authenticator identifies the caller, which on the Agent Platform is the
// `email` claim (the edge sets `x-user-id` from it, the controller's user-id
// claim is `email`), so a grant naming the Dex subject admits nobody.
package authorize

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/grant"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/signin"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

// Workspaces reads a Workspace for the caller in the context, after its
// Organization check, and records a sync request on it (workspace.Store).
type Workspaces interface {
	Get(ctx context.Context, name string) (*v1alpha1.Workspace, error)
	RequestSync(ctx context.Context, name string, at time.Time) (*v1alpha1.Workspace, error)
}

// Tokens are the people's sign-ins (signin.Store).
type Tokens interface {
	Access(ctx context.Context, person, instance string) (signin.Access, error)
}

// Config is what the check works with.
type Config struct {
	Workspaces Workspaces
	// Core reads the manifest's ConfigMap, the volume's claim and its
	// PersistentVolume, with the manager's ServiceAccount.
	Core kubernetes.Interface
	// Namespace is where the Workspaces and everything they own live.
	Namespace string
	// Instances are the installation's provider instances.
	Instances []provider.Instance
	// Tokens are the people's sign-ins.
	Tokens Tokens
	// ConnectURL is the page where a person connects an instance.
	ConnectURL func(instance string) string
	// Signer signs the grant.
	Signer *grant.Signer
	// Broker is the name kagent knows the manager's token endpoint by: every
	// provider host of a grant names it.
	Broker string
	// AgentNamespace is the namespace whose Agents may start a Session with
	// a grant, which every grant names.
	AgentNamespace string
	// CheckTimeout bounds the providers' answers about the person's access;
	// zero is DefaultCheckTimeout. A check that does not finish in time is a
	// refusal saying so, before the caller's own timeout ends the call.
	CheckTimeout time.Duration
	// RefreshAfter is the refresh threshold: a granted check on a workspace
	// whose last sync is older requests a sync; zero is DefaultRefreshAfter.
	RefreshAfter time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// DefaultRefreshAfter is the refresh threshold by default.
const DefaultRefreshAfter = time.Hour

// DefaultCheckTimeout is the access check's bound by default, well within
// muster's default tool timeout of 30 s.
const DefaultCheckTimeout = 20 * time.Second

// ErrNoEmail refuses a caller whose identity carries no email, which the
// grant names the person by: a grant without one admits nobody.
var ErrNoEmail = errors.New("the caller's identity has no email, which the grant names the person by")

// Authorizer runs the check.
type Authorizer struct {
	cfg       Config
	instances map[string]provider.Instance
}

// New validates cfg and builds the Authorizer.
func New(cfg Config) (*Authorizer, error) {
	var errs []error
	if cfg.Workspaces == nil || cfg.Core == nil || cfg.Tokens == nil || cfg.ConnectURL == nil {
		errs = append(errs, errors.New("authorize: workspaces, Kubernetes, the sign-in store and the connect URL are required"))
	}
	if cfg.Namespace == "" || cfg.Broker == "" {
		errs = append(errs, errors.New("authorize: the namespace and the broker name are required"))
	}
	if msgs := validation.IsDNS1123Label(cfg.AgentNamespace); len(msgs) > 0 {
		errs = append(errs, fmt.Errorf("authorize: the Agent namespace %q is no namespace name: %s", cfg.AgentNamespace, strings.Join(msgs, "; ")))
	}
	if cfg.CheckTimeout < 0 {
		errs = append(errs, fmt.Errorf("authorize: the check timeout must not be negative, got %s", cfg.CheckTimeout))
	}
	if cfg.CheckTimeout == 0 {
		cfg.CheckTimeout = DefaultCheckTimeout
	}
	if cfg.RefreshAfter < 0 {
		errs = append(errs, fmt.Errorf("authorize: the refresh threshold must not be negative, got %s", cfg.RefreshAfter))
	}
	if cfg.RefreshAfter == 0 {
		cfg.RefreshAfter = DefaultRefreshAfter
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Signer == nil {
		errs = append(errs, errors.New("authorize: no grant signer"))
	} else if err := cfg.Signer.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	instances := make(map[string]provider.Instance, len(cfg.Instances))
	for _, in := range cfg.Instances {
		instances[in.Name] = in
	}
	return &Authorizer{cfg: cfg, instances: instances}, nil
}

// Outcome is how a check ended.
type Outcome string

const (
	// Granted: the person reads every repository; Result.Grant is the grant.
	Granted Outcome = "granted"
	// ConnectRequired: the person has not connected every provider of the
	// workspace; Result.Connect has the links.
	ConnectRequired Outcome = "connect_required"
	// Refused: the person cannot read every repository (Result.Unreadable),
	// or the workspace cannot be started on yet (Result.Reason).
	Refused Outcome = "refused"
)

// Connect is a provider the person connects before the workspace is theirs.
type Connect struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
}

// Result is the check's answer.
type Result struct {
	Workspace string  `json:"workspace"`
	Outcome   Outcome `json:"outcome"`
	// Reason says why in a sentence, for every outcome but Granted.
	Reason string `json:"reason,omitempty"`
	// Grant is the signed grant, to be passed to start_session.
	Grant     string     `json:"grant,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Repositories are the workspace's repositories, `<owner>/<name>`.
	Repositories []string `json:"repositories,omitempty"`
	// Changed are the repositories pushed to since the last sync, which the
	// Session fetches before its first turn.
	Changed []string `json:"changed,omitempty"`
	// Unreadable are the repositories the person cannot read.
	Unreadable []string `json:"unreadable,omitempty"`
	// Connect are the providers the person has not connected.
	Connect []Connect `json:"connect,omitempty"`
	// SyncRequested is true when the check requested a sync of the
	// workspace, its last one being older than the refresh threshold; the
	// Session does not wait for it.
	SyncRequested bool `json:"syncRequested,omitempty"`
}

// Authorize checks the caller id against the workspace name: their sign-ins
// by id.Subject, the grant in their name by id.Email. An error is a failure
// to check (the workspace unknown or not the caller's Organization's, a caller
// without an email, a provider or the API server failing); every answer about
// the person's access is a Result.
func (a *Authorizer) Authorize(ctx context.Context, id *identity.Identity, name string) (*Result, error) {
	if id == nil || id.Subject == "" {
		return nil, errors.New("authorize: no caller")
	}
	if id.Email == "" {
		return nil, ErrNoEmail
	}
	ws, err := a.cfg.Workspaces.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	res := &Result{Workspace: name}
	refuse := func(format string, args ...any) (*Result, error) {
		res.Outcome, res.Reason = Refused, fmt.Sprintf(format, args...)
		return res, nil
	}
	if ws.DeletionTimestamp != nil {
		return refuse("workspace %s is being deleted", name)
	}
	manifest, err := a.manifest(ctx, ws)
	if err != nil {
		return nil, err
	}
	if manifest == nil {
		return refuse("workspace %s has not synced yet: its repositories are not on its volume", name)
	}

	// The manifest's live entries, by provider and owner.
	byProvider := map[string]map[string][]mirror.Entry{}
	for _, e := range manifest.Repositories {
		if e.DroppedAt != nil {
			continue
		}
		if byProvider[e.Provider] == nil {
			byProvider[e.Provider] = map[string][]mirror.Entry{}
		}
		byProvider[e.Provider][e.Owner] = append(byProvider[e.Provider][e.Owner], e)
		res.Repositories = append(res.Repositories, e.Key())
	}
	sort.Strings(res.Repositories)
	providers := sortedKeys(byProvider)
	for _, p := range providers {
		if _, ok := a.instances[p]; !ok {
			return refuse("workspace %s was synced from provider %s, which this installation no longer configures", name, p)
		}
	}

	volume, reason, err := a.volume(ctx, ws)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return refuse("workspace %s: %s", name, reason)
	}

	// The person's token per provider; every unconnected one is named at once.
	tokens := map[string]string{}
	for _, p := range providers {
		access, err := a.cfg.Tokens.Access(ctx, id.Subject, p)
		switch {
		case errors.Is(err, signin.ErrNotSignedIn), errors.Is(err, signin.ErrSignInExpired):
			res.Connect = append(res.Connect, Connect{Provider: p, URL: a.cfg.ConnectURL(p)})
		case err != nil:
			return nil, fmt.Errorf("sign-in to %s: %w", p, err)
		default:
			tokens[p] = access.Token
		}
	}
	if len(res.Connect) > 0 {
		return a.connectRequired(res, name)
	}

	// What the person reads, one check per owner, against the manifest, all
	// of it within the check's bound. A provider refusing the person's own
	// token (revoked there) is a connect.
	checkCtx, cancel := context.WithTimeout(ctx, a.cfg.CheckTimeout)
	defer cancel()
	for _, p := range providers {
		kind := a.instances[p].Kind
		cred := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tokens[p]})
		for _, owner := range sortedKeys(byProvider[p]) {
			entries := byProvider[p][owner]
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name
			}
			items, err := kind.Readable(checkCtx, cred, owner, names)
			if errors.Is(err, provider.ErrCredentialRefused) {
				res.Connect = append(res.Connect, Connect{Provider: p, URL: a.cfg.ConnectURL(p)})
				break
			}
			if err != nil && ctx.Err() == nil && errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
				res.Connect, res.Changed, res.Unreadable = nil, nil, nil
				return refuse("checking your access to workspace %s took longer than %s: %s did not answer for %s in time; try again",
					name, a.cfg.CheckTimeout, p, owner)
			}
			if err != nil {
				return nil, fmt.Errorf("check %s on %s: %w", owner, p, err)
			}
			readable := make(map[string]provider.Item, len(items))
			for _, it := range items {
				readable[strings.ToLower(it.Name)] = it
			}
			for _, e := range entries {
				it, ok := readable[strings.ToLower(e.Name)]
				switch {
				case !ok:
					res.Unreadable = append(res.Unreadable, e.Key())
				case it.LastChange.After(e.PushedAt):
					res.Changed = append(res.Changed, e.Key())
				}
			}
		}
	}
	if len(res.Connect) > 0 {
		return a.connectRequired(res, name)
	}
	if len(res.Unreadable) > 0 {
		sort.Strings(res.Unreadable)
		res.Changed = nil
		return refuse("you cannot read %d %s of workspace %s: %s", len(res.Unreadable),
			plural(len(res.Unreadable), "repository", "repositories"), name, strings.Join(res.Unreadable, ", "))
	}
	sort.Strings(res.Changed)

	g := grant.Grant{
		AgentNamespace: a.cfg.AgentNamespace,
		Workspace:      grant.Workspace{Namespace: a.cfg.Namespace, Name: name},
		Volume:         *volume,
		Mounts:         []grant.Mount{{SubPath: grant.SessionDirectory}, {SubPath: grant.Mirrors, ReadOnly: true}},
		Changed:        res.Changed,
		Providers:      a.providers(providers),
	}
	token, expiry, err := a.cfg.Signer.Sign(ctx, id.Email, g)
	if err != nil {
		return nil, err
	}
	res.Outcome, res.Grant, res.ExpiresAt = Granted, token, &expiry
	res.SyncRequested = a.refresh(ctx, ws)
	return res, nil
}

// refresh requests a sync of a workspace whose last sync is older than the
// refresh threshold, and reports whether it did. A sync that runs or is
// requested already is not requested again; a last cycle that only cleaned
// up session directories listed no provider, so it refreshed nothing. A
// failed request is logged: the grant stands, and the next Session start
// asks again.
func (a *Authorizer) refresh(ctx context.Context, ws *v1alpha1.Workspace) bool {
	if ws.Status.ActiveSync != nil || ws.Annotations[v1alpha1.AnnotationSyncRequested] != "" {
		return false
	}
	now := a.cfg.Now()
	if last := ws.Status.LastSync; last != nil && !cleanupOnly(last.Trigger) && now.Sub(last.Time.Time) < a.cfg.RefreshAfter {
		return false
	}
	if _, err := a.cfg.Workspaces.RequestSync(ctx, ws.Name, now); err != nil {
		slog.WarnContext(ctx, "workspace sync not requested", identity.LogAttr(ctx), "workspace", ws.Name, "error", err)
		return false
	}
	slog.InfoContext(ctx, "workspace sync requested: the last sync is older than the refresh threshold", identity.LogAttr(ctx),
		"workspace", ws.Name, "refreshAfter", a.cfg.RefreshAfter)
	return true
}

// cleanupOnly reports whether a cycle so triggered only cleans up session
// directories, listing and fetching nothing.
func cleanupOnly(t v1alpha1.SyncTrigger) bool {
	switch t {
	case v1alpha1.TriggerCleanup, v1alpha1.TriggerSessionDeleted, v1alpha1.TriggerDeletion:
		return true
	}
	return false
}

// connectRequired answers with the providers to connect, and nothing about
// what the person reads.
func (a *Authorizer) connectRequired(res *Result, name string) (*Result, error) {
	names := make([]string, len(res.Connect))
	for i, c := range res.Connect {
		names[i] = c.Provider
	}
	res.Outcome, res.Changed, res.Unreadable = ConnectRequired, nil, nil
	res.Reason = fmt.Sprintf("connect %s to use workspace %s: the Session works as you there", strings.Join(names, ", "), name)
	return res, nil
}

// manifest is the last sync's manifest; nil before the first sync.
func (a *Authorizer) manifest(ctx context.Context, ws *v1alpha1.Workspace) (*mirror.Manifest, error) {
	return workspace.ReadManifest(ctx, a.cfg.Core, a.cfg.Namespace, ws)
}

// volume is the workspace's volume by its CSI driver and handle; a reason
// instead while there is none.
func (a *Authorizer) volume(ctx context.Context, ws *v1alpha1.Workspace) (*grant.Volume, string, error) {
	ref, reason, err := workspace.Volume(ctx, a.cfg.Core, a.cfg.Namespace, ws)
	if ref == nil {
		return nil, reason, err
	}
	return &grant.Volume{Driver: ref.Driver, Handle: ref.Handle}, "", nil
}

// providers are the hosts of the named instances the Session gets the
// person's credential on: git and API alike, each from the broker.
func (a *Authorizer) providers(names []string) []grant.Provider {
	var out []grant.Provider
	for _, name := range names {
		hosts := a.instances[name].Hosts()
		for _, h := range slices.Concat(hosts.Git, hosts.API) {
			out = append(out, grant.Provider{Hostname: h.Name, PathPrefix: h.PathPrefix, Audience: name, Scheme: string(h.Scheme), Username: h.User, Broker: a.cfg.Broker})
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
