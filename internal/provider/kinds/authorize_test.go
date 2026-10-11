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
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/fake"
	"github.com/giantswarm/workspace-manager/internal/provider/github"
	"github.com/giantswarm/workspace-manager/internal/provider/github/githubtest"
	"github.com/giantswarm/workspace-manager/internal/signin"
)

const authNamespace = "kagent"

var authSynced = time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC)

// authWorkspaces is a workspace.Store of fixed Workspaces.
type authWorkspaces map[string]*v1alpha1.Workspace

func (w authWorkspaces) Get(_ context.Context, name string) (*v1alpha1.Workspace, error) {
	if ws, ok := w[name]; ok {
		return ws, nil
	}
	return nil, fmt.Errorf("get workspace %q: not found", name)
}

// authTokens are the people's sign-ins: person → instance → access token.
type authTokens map[string]map[string]string

func (t authTokens) Access(_ context.Context, person, instance string) (signin.Access, error) {
	tok, ok := t[person][instance]
	if !ok {
		return signin.Access{}, signin.ErrNotSignedIn
	}
	return signin.Access{Token: tok}, nil
}

// person is a caller: the Dex subject their sign-ins are keyed by, and the
// email the grant names them by.
func person(subject string) *identity.Identity {
	return &identity.Identity{Subject: subject, Email: subject + "@acme.test"}
}

type authFixture struct {
	authorizer *authorize.Authorizer
	keys       *grant.KeySource
	tokens     authTokens
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
	manifest, err := (&mirror.Manifest{SyncedAt: authSynced, Repositories: []mirror.Entry{
		{Provider: "github", Owner: "acme", Name: "api", PushedAt: authSynced.Add(-2 * time.Hour)},
		{Provider: "github", Owner: "acme", Name: "web", PushedAt: authSynced.Add(-time.Hour)},
		{Provider: "github", Owner: "acme", Name: "gone", PushedAt: dropped, DroppedAt: &dropped},
		{Provider: "code", Owner: "umbrella", Name: "theirs", PushedAt: authSynced.Add(-time.Hour)},
	}}).JSON()
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
	ready := v1alpha1.WorkspaceStatus{Manifest: "otter-manifest", Volume: &v1alpha1.VolumeRef{ClaimName: "otter"}}
	unsynced := ready
	unsynced.Manifest = ""
	unbound := ready
	unbound.Volume = &v1alpha1.VolumeRef{ClaimName: "pending"}
	ws := func(name string, status v1alpha1.WorkspaceStatus) *v1alpha1.Workspace {
		return &v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: authNamespace}, Status: status}
	}

	f := &authFixture{
		keys: &grant.KeySource{Client: cs, Namespace: "wm", Secret: "keys", Key: "grant"},
		tokens: authTokens{
			"alice": {"github": gh.Person("acme/api", "acme/web", "acme/other"), "code": backend.Person("umbrella/theirs")},
			"bob":   {"github": gh.Person("acme/api"), "code": backend.Person("umbrella/theirs")},
			"carol": {"github": gh.Person("acme/api", "acme/web")},
		},
	}
	f.authorizer, err = authorize.New(authorize.Config{
		Workspaces: authWorkspaces{"otter": ws("otter", ready), "fresh": ws("fresh", unsynced), "pending": ws("pending", unbound)},
		Core:       cs,
		Namespace:  authNamespace,
		Instances: []provider.Instance{
			{Name: "github", KindName: github.KindName, Kind: ghKind},
			{Name: "code", KindName: fake.KindName, Kind: fkKind},
		},
		Tokens:     f.tokens,
		ConnectURL: func(instance string) string { return "https://wm.example.com/connect/" + instance },
		Signer:     &grant.Signer{Keys: f.keys, Issuer: "https://wm.example.com", Audience: "kagent", TTL: 2 * time.Minute},
		Broker:     "workspace-manager",
	})
	require.NoError(t, err)
	return f
}

func TestAPersonReadingEverythingGetsAGrant(t *testing.T) {
	f := authSetup(t)
	before := time.Now().Truncate(time.Second)
	res, err := f.authorizer.Authorize(context.Background(), person("alice"), "otter")
	require.NoError(t, err)
	require.Equal(t, authorize.Granted, res.Outcome, res.Reason)
	assert.Equal(t, []string{"acme/api", "acme/web", "umbrella/theirs"}, res.Repositories, "the dropped mirror is no repository of the workspace")
	assert.Equal(t, []string{"acme/api"}, res.Changed, "exactly the repository pushed to since the last sync")
	assert.Empty(t, res.Unreadable)
	require.NotNil(t, res.ExpiresAt)
	assert.WithinDuration(t, before.Add(2*time.Minute), *res.ExpiresAt, 2*time.Second, "expires as configured")

	claims := verified(t, f.keys, res.Grant)
	assert.Equal(t, "alice@acme.test", claims.Subject, "the grant names the person by email, not by the sign-in key")
	assert.Equal(t, grant.Workspace{Namespace: authNamespace, Name: "otter"}, claims.Workspace)
	assert.Equal(t, grant.Volume{Driver: "nfs.csi.k8s.io", Handle: "nfs-server#share#pvc-otter"}, claims.Volume)
	assert.Equal(t, []grant.Mount{{SubPath: "sessions/${SESSION_ID}"}, {SubPath: "mirrors", ReadOnly: true}}, claims.Mounts)
	assert.Equal(t, []string{"acme/api"}, claims.Changed)
	var audiences []string
	hosts := map[string]bool{}
	for _, p := range claims.Providers {
		assert.False(t, hosts[p.Hostname], "%s once: kagent's egress refuses two credentials for one host", p.Hostname)
		hosts[p.Hostname] = true
		audiences = append(audiences, p.Audience)
		assert.Equal(t, "workspace-manager", p.Broker)
		assert.Equal(t, p.Scheme == "Basic", p.Username != "", "%s: a user name with Basic only", p.Hostname)
	}
	assert.Contains(t, audiences, "github")
	assert.Contains(t, audiences, "code")

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
	res, err := f.authorizer.Authorize(context.Background(), &identity.Identity{Subject: "alice", Email: "alice@acme.test"}, "otter")
	require.NoError(t, err)
	require.Equal(t, authorize.Granted, res.Outcome, res.Reason)
	sub := verified(t, f.keys, res.Grant).Subject
	for creator, admitted := range map[string]bool{"alice@acme.test": true, "alice": false, "bob@acme.test": false} {
		assert.Equal(t, admitted, sub == creator, "creator %q against the grant's sub %q", creator, sub)
	}

	res, err = f.authorizer.Authorize(context.Background(), &identity.Identity{Subject: "alice"}, "otter")
	require.ErrorIs(t, err, authorize.ErrNoEmail)
	assert.Nil(t, res)
}

func TestAPersonMissingARepositoryIsRefusedWithItsName(t *testing.T) {
	res, err := authSetup(t).authorizer.Authorize(context.Background(), person("bob"), "otter")
	require.NoError(t, err)
	assert.Equal(t, authorize.Refused, res.Outcome)
	assert.Equal(t, []string{"acme/web"}, res.Unreadable)
	assert.Contains(t, res.Reason, "acme/web")
	assert.Empty(t, res.Grant)
	assert.Empty(t, res.Changed)
}

func TestAPersonNotConnectedGetsTheConnectLink(t *testing.T) {
	res, err := authSetup(t).authorizer.Authorize(context.Background(), person("carol"), "otter")
	require.NoError(t, err)
	assert.Equal(t, authorize.ConnectRequired, res.Outcome)
	assert.Equal(t, []authorize.Connect{{Provider: "code", URL: "https://wm.example.com/connect/code"}}, res.Connect)
	assert.Contains(t, res.Reason, "connect code")
	assert.Empty(t, res.Grant)

	res, err = authSetup(t).authorizer.Authorize(context.Background(), person("dave"), "otter")
	require.NoError(t, err)
	assert.Len(t, res.Connect, 2, "every provider the person has not connected, at once")
}

func TestAWorkspaceNotSyncedYetIsRefused(t *testing.T) {
	f := authSetup(t)
	res, err := f.authorizer.Authorize(context.Background(), person("alice"), "fresh")
	require.NoError(t, err)
	assert.Equal(t, authorize.Refused, res.Outcome)
	assert.Contains(t, res.Reason, "has not synced yet")
	assert.Empty(t, res.Grant)

	res, err = f.authorizer.Authorize(context.Background(), person("alice"), "pending")
	require.NoError(t, err)
	assert.Equal(t, authorize.Refused, res.Outcome)
	assert.Contains(t, res.Reason, "not bound yet")
}

func TestAnUnknownWorkspaceIsAnError(t *testing.T) {
	_, err := authSetup(t).authorizer.Authorize(context.Background(), person("alice"), "nope")
	require.Error(t, err)
}

func TestASignInTheProviderRefusesAsksToConnectAgain(t *testing.T) {
	f := authSetup(t)
	f.tokens["alice"]["github"] = "revoked"
	res, err := f.authorizer.Authorize(context.Background(), person("alice"), "otter")
	require.NoError(t, err)
	assert.Equal(t, authorize.ConnectRequired, res.Outcome)
	assert.Equal(t, []authorize.Connect{{Provider: "github", URL: "https://wm.example.com/connect/github"}}, res.Connect)
	assert.Empty(t, res.Grant)
	assert.Empty(t, res.Changed)
}
