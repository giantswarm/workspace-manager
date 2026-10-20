package kinds

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/authorize"
	"github.com/giantswarm/workspace-manager/internal/grant"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/fake"
	"github.com/giantswarm/workspace-manager/internal/provider/github"
	"github.com/giantswarm/workspace-manager/internal/provider/github/githubtest"
	"github.com/giantswarm/workspace-manager/internal/provider/gitlab"
	"github.com/giantswarm/workspace-manager/internal/provider/gitlab/gitlabtest"
)

const authNamespace = "kagent"

// agentNamespace is the grant's Agent namespace, apart from the workspaces'.
const agentNamespace = "agents"

var authSynced = time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC)

// authWorkspaces is a workspace.Store of fixed Workspaces.
type authWorkspaces map[string]*v1alpha1.Workspace

func (w authWorkspaces) Get(_ context.Context, name string) (*v1alpha1.Workspace, error) {
	if ws, ok := w[name]; ok {
		return ws, nil
	}
	return nil, fmt.Errorf("get workspace %q: not found", name)
}

// RequestSync records the request as the store does, by its annotation.
func (w authWorkspaces) RequestSync(ctx context.Context, name string, at time.Time) (*v1alpha1.Workspace, error) {
	ws, err := w.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	ws.Annotations[v1alpha1.AnnotationSyncRequested] = at.UTC().Format(time.RFC3339)
	return ws, nil
}

// authTokens are the tokens muster attaches to a person's call: person →
// instance → access token.
type authTokens map[string]map[string]string

// as is the context of a call by person, carrying their tokens.
func (t authTokens) as(person string) context.Context {
	ctx := context.Background()
	for instance, tok := range t[person] {
		ctx = identity.ContextWithProviderToken(ctx, instance, tok)
	}
	return ctx
}

// grants are the test's muster registration, receiving both instances' Apps.
var grants = muster.Grants{Apps: []string{"github", "code"}, Server: "workspace-manager"}

// person is a caller: the Dex subject, and the email the grant names them
// by.
func person(subject string) *identity.Identity {
	return &identity.Identity{Subject: subject, Email: subject + "@acme.test"}
}

type authFixture struct {
	authorizer *authorize.Authorizer
	core       *k8sfake.Clientset
	instances  []provider.Instance
	workspaces authWorkspaces
	keys       *grant.KeySource
	tokens     authTokens
	code       provider.Kind
}

// authSetup is a workspace on two providers at once: GitHub (the stub API) with
// acme/api, pushed to after the last sync, and acme/web; and the fake kind
// with umbrella/theirs. A dropped mirror, acme/gone, nobody reads.
func authSetup(t *testing.T) *authFixture {
	t.Helper()
	gh := githubtest.NewServer(t)
	gh.Put(provider.Item{Owner: "acme", Name: "api", Language: "Go", LastChange: authSynced.Add(3 * time.Hour)})
	gh.Put(provider.Item{Owner: "acme", Name: "web", Language: "TypeScript", LastChange: authSynced.Add(-time.Hour)})
	ghKind, err := github.Factory(github.Options{Clock: gh.Clock})(gh.Values())
	require.NoError(t, err)
	backend := fake.NewBackend()
	backend.Put(provider.Item{Owner: "umbrella", Name: "theirs", LastChange: authSynced.Add(-time.Hour)})
	fkKind, err := fake.Factory(backend.Store)(backend.Values())
	require.NoError(t, err)

	dropped := authSynced.Add(-24 * time.Hour)
	cs, keys := authCluster(t, []mirror.Entry{
		{Provider: "github", Owner: "acme", Name: "api", PushedAt: authSynced.Add(-2 * time.Hour)},
		{Provider: "github", Owner: "acme", Name: "web", PushedAt: authSynced.Add(-time.Hour)},
		{Provider: "github", Owner: "acme", Name: "gone", PushedAt: dropped, DroppedAt: &dropped},
		{Provider: "code", Owner: "umbrella", Name: "theirs", PushedAt: authSynced.Add(-time.Hour)},
	})
	unsynced := authReady
	unsynced.Manifest = ""
	unbound := authReady
	unbound.Volume = &v1alpha1.VolumeRef{ClaimName: "pending"}

	f := &authFixture{
		keys: keys,
		code: fkKind,
		core: cs,
		instances: []provider.Instance{
			{Name: "github", KindName: github.KindName, Kind: ghKind},
			{Name: "code", KindName: fake.KindName, Kind: fkKind},
		},
		tokens: authTokens{
			"alice": {"github": gh.Person("acme/api", "acme/web", "acme/other"), "code": backend.Person("umbrella/theirs")},
			"bob":   {"github": gh.Person("acme/api"), "code": backend.Person("umbrella/theirs")},
			"carol": {"github": gh.Person("acme/api", "acme/web")},
		},
	}
	f.workspaces = authWorkspaces{"otter": authWorkspace("otter", authReady), "fresh": authWorkspace("fresh", unsynced), "pending": authWorkspace("pending", unbound)}
	f.authorizer, err = authorize.New(authorize.Config{
		Workspaces:     f.workspaces,
		Core:           cs,
		Namespace:      authNamespace,
		Instances:      f.instances,
		Grants:         grants,
		Signer:         &grant.Signer{Keys: f.keys, Issuer: "https://wm.example.com", Audience: "kagent", TTL: 2 * time.Minute},
		Broker:         "workspace-manager",
		AgentNamespace: agentNamespace,
	})
	require.NoError(t, err)
	return f
}

// authReady is a synced workspace's status: authCluster's manifest and
// bound volume.
var authReady = v1alpha1.WorkspaceStatus{Manifest: "otter-manifest", Volume: &v1alpha1.VolumeRef{ClaimName: "otter"}}

func authWorkspace(name string, status v1alpha1.WorkspaceStatus) *v1alpha1.Workspace {
	return &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: authNamespace}, Status: status}
}

// authCluster holds a last sync of entries (authReady's manifest), the
// volume it is on, a claim not bound yet ("pending") and the grant's signing
// key.
func authCluster(t *testing.T, entries []mirror.Entry) (*k8sfake.Clientset, *grant.KeySource) {
	t.Helper()
	manifest, err := (&mirror.Manifest{SyncedAt: authSynced, Repositories: entries}).JSON()
	require.NoError(t, err)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(ecKey)
	require.NoError(t, err)
	cs := k8sfake.NewClientset(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "otter-manifest", Namespace: authNamespace}, Data: map[string]string{mirror.ManifestFile: string(manifest)}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "otter", Namespace: authNamespace}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-otter"}},
		&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-otter"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "nfs.csi.k8s.io", VolumeHandle: "nfs-server#share#pvc-otter"}}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: authNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keys", Namespace: "wm"},
			Data: map[string][]byte{"grant": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}},
	)
	return cs, &grant.KeySource{Client: cs, Namespace: "wm", Secret: "keys", Key: "grant"}
}

func TestAPersonReadingEverythingGetsAGrant(t *testing.T) {
	f := authSetup(t)
	before := time.Now().Truncate(time.Second)
	res, err := f.authorizer.Authorize(f.tokens.as("alice"), person("alice"), "otter")
	require.NoError(t, err)
	require.Equal(t, authorize.Granted, res.Outcome, res.Reason)
	assert.Equal(t, []string{"acme/api", "acme/web", "umbrella/theirs"}, res.Repositories, "the dropped mirror is no repository of the workspace")
	assert.Equal(t, []string{"acme/api"}, res.Changed, "exactly the repository pushed to since the last sync")
	assert.Empty(t, res.Unreadable)
	require.NotNil(t, res.ExpiresAt)
	assert.WithinDuration(t, before.Add(2*time.Minute), *res.ExpiresAt, 2*time.Second, "expires as configured")

	claims := verified(t, f.keys, res.Grant)
	assert.Equal(t, "alice@acme.test", claims.Subject, "the grant names the person by email, not by the Dex subject")
	assert.Equal(t, agentNamespace, claims.AgentNamespace, "the grant names the namespace whose Agents it admits")
	assert.Equal(t, grant.Workspace{Namespace: authNamespace, Name: "otter"}, claims.Workspace)
	assert.Equal(t, grant.Volume{Driver: "nfs.csi.k8s.io", Handle: "nfs-server#share#pvc-otter"}, claims.Volume)
	assert.Equal(t, []grant.Mount{{SubPath: "sessions/${SESSION_ID}"}, {SubPath: "mirrors", ReadOnly: true}}, claims.Mounts)
	assert.Equal(t, []string{"acme/api"}, claims.Changed)
	var audiences []string
	hosts := map[string]bool{}
	for _, p := range claims.Providers {
		key := p.Hostname + p.PathPrefix
		assert.False(t, hosts[key], "%s once: kagent's egress refuses two credentials for one host and path prefix", key)
		hosts[key] = true
		audiences = append(audiences, p.Audience)
		assert.Equal(t, "workspace-manager", p.Broker)
		assert.Equal(t, p.Scheme == "Basic", p.Username != "", "%s: a user name with Basic only", key)
	}
	assert.Contains(t, audiences, "github")
	assert.Contains(t, audiences, "code")
	var code []grant.Provider
	for _, p := range claims.Providers {
		if p.Audience == "code" {
			code = append(code, p)
		}
	}
	host := f.code.Hosts().Git[0].Name
	assert.ElementsMatch(t, []grant.Provider{
		{Hostname: host, Provider: "code", Audience: "code", Scheme: "Basic", Username: "oauth2", Broker: "workspace-manager"},
		{Hostname: host, PathPrefix: "/api/", Provider: "code", Audience: "code", Scheme: "Bearer", Broker: "workspace-manager"},
	}, code, "one host: Basic for git, Bearer under the API's path prefix")

	for _, token := range f.tokens["alice"] {
		assert.NotContains(t, res.Grant, token, "no token in the grant")
		raw, err := json.Marshal(claims)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), token, "no token in the grant's claims")
	}
}

// verified parses a grant as kagent's admission does: signed by a key the
// manager publishes, by this issuer, for this audience, in time.
func verified(t *testing.T, keys *grant.KeySource, token string) grant.Claims {
	t.Helper()
	srv := httptest.NewServer(grant.JWKS(keys, nil))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var set jose.JSONWebKeySet
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&set))
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	require.NoError(t, err)
	published := set.Key(parsed.Headers[0].KeyID)
	require.Len(t, published, 1, "the grant's key is published")
	var claims grant.Claims
	require.NoError(t, parsed.Claims(published[0].Key, &claims))
	require.NoError(t, claims.Validate(jwt.Expected{Issuer: "https://wm.example.com", AnyAudience: jwt.Audience{"kagent"}}))
	return claims
}

// kagent's admission compares the verified grant's sub with the Session's
// creator as its authenticator names the caller: on the platform the email.
// A caller the manager knows by Dex subject only gets no grant, since one
// naming the subject would admit nobody.
func TestTheGrantNamesTheCallerAsKagentNamesTheCreator(t *testing.T) {
	f := authSetup(t)
	res, err := f.authorizer.Authorize(f.tokens.as("alice"), &identity.Identity{Subject: "alice", Email: "alice@acme.test"}, "otter")
	require.NoError(t, err)
	require.Equal(t, authorize.Granted, res.Outcome, res.Reason)
	sub := verified(t, f.keys, res.Grant).Subject
	for creator, admitted := range map[string]bool{"alice@acme.test": true, "alice": false, "bob@acme.test": false} {
		assert.Equal(t, admitted, sub == creator, "creator %q against the grant's sub %q", creator, sub)
	}

	res, err = f.authorizer.Authorize(f.tokens.as("alice"), &identity.Identity{Subject: "alice"}, "otter")
	require.ErrorIs(t, err, authorize.ErrNoEmail)
	assert.Nil(t, res)
}

// A granted check requests a sync once the last one is older than the
// refresh threshold (an hour by default), and the grant does not wait for
// it: the mirrors are refreshed at Session start, never on a schedule.
func TestAGrantPastTheRefreshThresholdRequestsASync(t *testing.T) {
	now := time.Now()
	for name, c := range map[string]struct {
		status    func(*v1alpha1.WorkspaceStatus)
		requested bool
	}{
		"never synced": {func(s *v1alpha1.WorkspaceStatus) {}, true},
		"synced within the hour": {func(s *v1alpha1.WorkspaceStatus) {
			s.LastSync = lastSync(now.Add(-59*time.Minute), v1alpha1.TriggerChange)
		}, false},
		"synced over an hour ago": {func(s *v1alpha1.WorkspaceStatus) {
			s.LastSync = lastSync(now.Add(-61*time.Minute), v1alpha1.TriggerRequest)
		}, true},
		"only cleaned up since": {func(s *v1alpha1.WorkspaceStatus) {
			s.LastSync = lastSync(now.Add(-time.Minute), v1alpha1.TriggerCleanup)
		}, true},
		"a Session deleted since": {func(s *v1alpha1.WorkspaceStatus) {
			s.LastSync = lastSync(now.Add(-time.Minute), v1alpha1.TriggerSessionDeleted)
		}, true},
		"a sync running":           {func(s *v1alpha1.WorkspaceStatus) { s.ActiveSync = &v1alpha1.ActiveSync{Job: "otter-sync-ab12"} }, false},
		"an old cycle's last sync": {func(s *v1alpha1.WorkspaceStatus) { s.LastSync = lastSync(now.Add(-2*time.Hour), "schedule") }, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := authSetup(t)
			ws := f.workspaces["otter"]
			c.status(&ws.Status)
			res, err := f.authorizer.Authorize(f.tokens.as("alice"), person("alice"), "otter")
			require.NoError(t, err)
			require.Equal(t, authorize.Granted, res.Outcome, res.Reason)
			assert.NotEmpty(t, res.Grant, "the grant does not wait for the sync")
			assert.Equal(t, c.requested, res.SyncRequested)
			assert.Equal(t, c.requested, ws.Annotations[v1alpha1.AnnotationSyncRequested] != "")

			if c.requested {
				res, err = f.authorizer.Authorize(f.tokens.as("alice"), person("alice"), "otter")
				require.NoError(t, err)
				assert.False(t, res.SyncRequested, "a sync requested already is not requested again")
			}
		})
	}
}

func TestARefusedCheckRequestsNoSync(t *testing.T) {
	f := authSetup(t)
	res, err := f.authorizer.Authorize(f.tokens.as("bob"), person("bob"), "otter")
	require.NoError(t, err)
	require.Equal(t, authorize.Refused, res.Outcome)
	assert.False(t, res.SyncRequested)
	assert.Empty(t, f.workspaces["otter"].Annotations[v1alpha1.AnnotationSyncRequested])
}

func lastSync(at time.Time, trigger v1alpha1.SyncTrigger) *v1alpha1.LastSync {
	return &v1alpha1.LastSync{Time: metav1.NewTime(at), Result: v1alpha1.SyncSucceeded, Trigger: trigger}
}

func TestAPersonMissingARepositoryIsRefusedWithItsName(t *testing.T) {
	f := authSetup(t)
	res, err := f.authorizer.Authorize(f.tokens.as("bob"), person("bob"), "otter")
	require.NoError(t, err)
	assert.Equal(t, authorize.Refused, res.Outcome)
	assert.Equal(t, []string{"acme/web"}, res.Unreadable)
	assert.Contains(t, res.Reason, "acme/web")
	assert.Empty(t, res.Grant)
	assert.Empty(t, res.Changed)
}

func TestAPersonWithoutAGrantIsToldToGrantInMuster(t *testing.T) {
	f := authSetup(t)
	res, err := f.authorizer.Authorize(f.tokens.as("carol"), person("carol"), "otter")
	require.NoError(t, err)
	assert.Equal(t, authorize.ConnectRequired, res.Outcome)
	assert.Equal(t, []authorize.Connect{{Provider: "code", SignIn: "grant the App of provider code once in muster: core_auth_login app=code"}}, res.Connect)
	assert.Equal(t, muster.StatusGrantRequired, res.Status, "muster answers it with the App's sign-in link")
	assert.Equal(t, []string{"code"}, res.Apps, "only the App not granted")
	assert.Contains(t, res.Reason, "grant code in muster")
	assert.Empty(t, res.Grant)

	f = authSetup(t)
	res, err = f.authorizer.Authorize(f.tokens.as("dave"), person("dave"), "otter")
	require.NoError(t, err)
	assert.Len(t, res.Connect, 2, "every provider the person has not granted, at once")
	assert.Equal(t, []string{"code", "github"}, res.Apps)
}

// A registration pinned to one instance's App names how to grant it, and
// that muster holds no grant of the other; no grant_required, which only
// Apps muster keeps answer.
func TestAPinnedRegistrationNamesItsOneApp(t *testing.T) {
	f := authSetup(t)
	pinned := muster.Grants{Instance: "github", Server: "workspace-manager"}
	a, err := authorize.New(authorize.Config{
		Workspaces: f.workspaces, Core: f.core, Namespace: authNamespace, Instances: f.instances, Grants: pinned,
		Signer: &grant.Signer{Keys: f.keys, Issuer: "https://wm.example.com", Audience: "kagent", TTL: 2 * time.Minute},
		Broker: "workspace-manager", AgentNamespace: agentNamespace,
	})
	require.NoError(t, err)
	res, err := a.Authorize(f.tokens.as("dave"), person("dave"), "otter")
	require.NoError(t, err)
	assert.Equal(t, authorize.ConnectRequired, res.Outcome)
	assert.Equal(t, []authorize.Connect{
		{Provider: "code", SignIn: "muster holds no grant for provider code: the workspace-manager's registration in muster names no App of it"},
		{Provider: "github", SignIn: "grant the App of provider github once in muster: core_auth_login server=workspace-manager"},
	}, res.Connect)
	assert.Empty(t, res.Status)
	assert.Empty(t, res.Apps)
}

func TestAWorkspaceNotSyncedYetIsRefused(t *testing.T) {
	f := authSetup(t)
	res, err := f.authorizer.Authorize(f.tokens.as("alice"), person("alice"), "fresh")
	require.NoError(t, err)
	assert.Equal(t, authorize.Refused, res.Outcome)
	assert.Contains(t, res.Reason, "has not synced yet")
	assert.Empty(t, res.Grant)

	res, err = f.authorizer.Authorize(f.tokens.as("alice"), person("alice"), "pending")
	require.NoError(t, err)
	assert.Equal(t, authorize.Refused, res.Outcome)
	assert.Contains(t, res.Reason, "not bound yet")
}

func TestAnUnknownWorkspaceIsAnError(t *testing.T) {
	f := authSetup(t)
	_, err := f.authorizer.Authorize(f.tokens.as("alice"), person("alice"), "nope")
	require.Error(t, err)
}

func TestATokenTheProviderRefusesAsksToGrantAgain(t *testing.T) {
	f := authSetup(t)
	f.tokens["alice"]["github"] = "revoked"
	res, err := f.authorizer.Authorize(f.tokens.as("alice"), person("alice"), "otter")
	require.NoError(t, err)
	assert.Equal(t, authorize.ConnectRequired, res.Outcome)
	assert.Equal(t, []authorize.Connect{{Provider: "github", SignIn: "grant the App of provider github once in muster: core_auth_login app=github"}}, res.Connect)
	assert.Equal(t, []string{"github"}, res.Apps)
	assert.Empty(t, res.Grant)
	assert.Empty(t, res.Changed)
}

// A workspace selecting a few repositories of an organization holding
// thousands is checked in what it selects: GitHub answering each request in
// 1.5 s, as from afar, the listing of 2,000 repositories alone would take
// 20 pages, 30 s one after the other, muster's whole tool timeout.
func TestALargeOrganizationIsCheckedInWhatTheWorkspaceSelects(t *testing.T) {
	const latency = 1500 * time.Millisecond
	for _, tc := range []struct {
		name         string
		selected     int
		unreadable   []string
		checkTimeout time.Duration
		outcome      authorize.Outcome
		reason       string
		listed       int
		within       time.Duration
	}{
		{name: "one repository", selected: 1, outcome: authorize.Granted, within: 2 * latency},
		{name: "one repository the person cannot read is refused by name", selected: 2, unreadable: []string{"acme/r0001"},
			outcome: authorize.Refused, reason: "you cannot read 1 repository of workspace big: acme/r0001", within: 2 * latency},
		{name: "more repositories than a page holds take the listing, its pages at once", selected: 150,
			outcome: authorize.Granted, listed: 20, within: 5 * latency},
		{name: "a check past its bound is refused, saying so", selected: 1, checkTimeout: latency / 3, outcome: authorize.Refused,
			reason: "checking your access to workspace big took longer than 500ms: github did not answer for acme in time; try again", within: latency},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gh := githubtest.NewServer(t)
			gh.PageSize = 100
			unreadable := map[string]bool{}
			for _, key := range tc.unreadable {
				unreadable[key] = true
			}
			var reads []string
			var entries []mirror.Entry
			for i := range 2000 {
				name := fmt.Sprintf("r%04d", i)
				gh.Put(provider.Item{Owner: "acme", Name: name, LastChange: authSynced.Add(-time.Hour)})
				if !unreadable["acme/"+name] {
					reads = append(reads, "acme/"+name)
				}
				if i < tc.selected {
					entries = append(entries, mirror.Entry{Provider: "github", Owner: "acme", Name: name, PushedAt: authSynced.Add(-time.Hour)})
				}
			}
			ghKind, err := github.Factory(github.Options{Clock: gh.Clock})(gh.Values())
			require.NoError(t, err)
			cs, keys := authCluster(t, entries)
			a, err := authorize.New(authorize.Config{
				Workspaces:     authWorkspaces{"big": authWorkspace("big", authReady)},
				Core:           cs,
				Namespace:      authNamespace,
				Instances:      []provider.Instance{{Name: "github", KindName: github.KindName, Kind: ghKind}},
				Grants:         grants,
				Signer:         &grant.Signer{Keys: keys, Issuer: "https://wm.example.com", Audience: "kagent", TTL: 2 * time.Minute},
				Broker:         "workspace-manager",
				AgentNamespace: agentNamespace,
				CheckTimeout:   tc.checkTimeout,
			})
			require.NoError(t, err)

			alice := identity.ContextWithProviderToken(context.Background(), "github", gh.Person(reads...))
			gh.Latency = latency
			start := time.Now()
			res, err := a.Authorize(alice, person("alice"), "big")
			took := time.Since(start)
			t.Logf("%d of 2,000 repositories selected: %s in %s, %d listing pages", tc.selected, res.Outcome, took.Round(time.Millisecond), gh.Listed())
			require.NoError(t, err)
			assert.Equal(t, tc.outcome, res.Outcome, res.Reason)
			assert.Equal(t, tc.reason, res.Reason)
			assert.Equal(t, tc.unreadable, res.Unreadable)
			assert.Equal(t, tc.listed, gh.Listed(), "listing pages")
			assert.Less(t, took, tc.within)
			assert.Equal(t, tc.outcome == authorize.Granted, res.Grant != "", "a grant only when granted")
		})
	}
}

// A workspace selecting a project of a GitLab group holding thousands is
// checked in what it selects: GitLab answering each request in 1.5 s, the
// person's listing of 2,000 projects and the project's head would take 21
// round trips, 31.5 s one after the other, past muster's tool timeout.
func TestALargeGitLabGroupIsCheckedInWhatTheWorkspaceSelects(t *testing.T) {
	const latency = 1500 * time.Millisecond
	gl := gitlabtest.NewServer(t)
	gl.PageSize = 100
	var reads []string
	for i := range 2000 {
		name := fmt.Sprintf("team~r%04d", i)
		gl.Put(provider.Item{Owner: "acme", Name: name, LastChange: authSynced.Add(-time.Hour)})
		reads = append(reads, "acme/"+name)
	}
	glKind, err := gitlab.Factory(gitlab.Options{Clock: gl.Clock})(gl.Values())
	require.NoError(t, err)
	cs, keys := authCluster(t, []mirror.Entry{{Provider: "gitlab", Owner: "acme", Name: "team~r0042", PushedAt: authSynced.Add(-time.Hour)}})
	a, err := authorize.New(authorize.Config{
		Workspaces:     authWorkspaces{"big": authWorkspace("big", authReady)},
		Core:           cs,
		Namespace:      authNamespace,
		Instances:      []provider.Instance{{Name: "gitlab", KindName: gitlab.KindName, Kind: glKind}},
		Grants:         muster.Grants{Instance: "gitlab", Server: "workspace-manager"},
		Signer:         &grant.Signer{Keys: keys, Issuer: "https://wm.example.com", Audience: "kagent", TTL: 2 * time.Minute},
		Broker:         "workspace-manager",
		AgentNamespace: agentNamespace,
	})
	require.NoError(t, err)

	alice := identity.ContextWithProviderToken(context.Background(), "gitlab", gl.Person(reads...))
	gl.Latency = latency
	start := time.Now()
	res, err := a.Authorize(alice, person("alice"), "big")
	took := time.Since(start)
	t.Logf("1 of 2,000 projects selected: %s in %s, %d listing pages", res.Outcome, took.Round(time.Millisecond), gl.Listed())
	require.NoError(t, err)
	assert.Equal(t, authorize.Granted, res.Outcome, res.Reason)
	assert.NotEmpty(t, res.Grant)
	assert.Zero(t, gl.Listed(), "no listing for one project")
	assert.Less(t, took, 3*latency, "the project and its head")
}
