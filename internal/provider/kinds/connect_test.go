package kinds

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/connect"
	"github.com/giantswarm/workspace-manager/internal/connect/connecttest"
	"github.com/giantswarm/workspace-manager/internal/provider/fake"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
	"github.com/giantswarm/workspace-manager/internal/server"
	"github.com/giantswarm/workspace-manager/internal/signin"
)

const managerNamespace = "workspace-manager"

// connectEnv is a manager with a GitHub instance and a fake-kind instance
// configured at once, each behind its own fake authorization server, its
// browser pages served through the server's routes.
type connectEnv struct {
	github, code *connecttest.AuthServer
	dex          *connecttest.Dex
	conn         *connect.Connector
	store        *signin.KubeStore
	kube         *k8sfake.Clientset
	manager      string
	skew         atomic.Int64 // added to the connector's clock
}

func newConnectEnv(t *testing.T) *connectEnv {
	t.Helper()
	e := &connectEnv{
		github: connecttest.NewAuthServer(t, "Iv1.github", "github-client-secret"),
		code:   connecttest.NewAuthServer(t, "fake-client", "fake-client-secret"),
		dex:    connecttest.NewDex(t),
		kube:   k8sfake.NewClientset(),
	}
	cfg := fmt.Sprintf(`providers:
- name: github
  kind: github
  values:
    url: %s
    app: {id: "1", privateKey: {name: github-app, key: private-key}}
    oauth: {clientID: Iv1.github, clientSecret: {name: github-oauth, key: client-secret}}
- name: code
  kind: fake
  values:
    host: %s
    token: {name: fake-sync, key: token}
    clientID: fake-client
    clientSecret: {name: fake-oauth, key: client-secret}
`, e.github.URL, e.code.Listener.Addr().String())
	instances, err := testRegistry(fake.NewStore("sync")).Load([]byte(cfg))
	require.NoError(t, err)
	secrets := providertest.Secrets{
		{Name: "github-oauth", Key: "client-secret"}: []byte("github-client-secret"),
		{Name: "fake-oauth", Key: "client-secret"}:   []byte("fake-client-secret"),
	}

	ts := httptest.NewUnstartedServer(nil)
	ts.StartTLS()
	t.Cleanup(ts.Close)
	e.manager = ts.URL
	e.dex.RedirectURL = ts.URL + connect.SignInPath

	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)
	keyring, err := signin.NewKeyring(map[string][]byte{"k1": key}, "k1")
	require.NoError(t, err)
	clients, err := connect.NewClients(instances, secrets, ts.URL)
	require.NoError(t, err)
	e.store, err = signin.NewKubeStore(signin.Options{Client: e.kube, Namespace: managerNamespace, Keyring: keyring, OAuth2: clients, Identity: "test"})
	require.NoError(t, err)
	e.conn, err = connect.New(connect.Options{Clients: clients, Store: e.store, Keyring: keyring, HTTPClient: e.github.Client(),
		Now: func() time.Time { return time.Now().Add(time.Duration(e.skew.Load())) }})
	require.NoError(t, err)
	pages, err := connect.NewPages(e.conn, e.dex)
	require.NoError(t, err)
	srv, err := server.New(server.Config{Pages: pages}, api.NewMCPServer(api.Config{Providers: instances, Connector: e.conn}, "test"), nil)
	require.NoError(t, err)
	ts.Config.Handler = srv.Handler()
	return e
}

// get opens u in browser and returns the final page's status and text.
func get(t *testing.T, browser *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := browser.Get(u)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// authorize asks the provider for a code without following its redirect to
// the callback: the callback URL it would send the browser to.
func (e *connectEnv) authorize(t *testing.T, authURL string) *url.URL {
	t.Helper()
	c := &http.Client{Transport: e.github.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authURL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	return loc
}

func (e *connectEnv) connected(t *testing.T, person string) map[string]bool {
	t.Helper()
	list, err := e.conn.Providers(context.Background(), person)
	require.NoError(t, err)
	out := map[string]bool{}
	for _, s := range list {
		out[s.Name] = s.Connected
	}
	return out
}

func (e *connectEnv) signIns(t *testing.T) [][]byte {
	t.Helper()
	list, err := e.kube.CoreV1().Secrets(managerNamespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	var out [][]byte
	for _, s := range list.Items {
		if s.Type == signin.SecretType {
			out = append(out, s.Data["sealed"])
		}
	}
	return out
}

func TestConnectProvidersIndependently(t *testing.T) {
	e := newConnectEnv(t)
	ctx := context.Background()
	alice := connecttest.NewBrowser(t, e.dex, "alice")

	assert.Equal(t, map[string]bool{"github": false, "code": false}, e.connected(t, "alice"))

	// The connect page signs the browser in, then runs the provider's flow.
	status, page := get(t, alice, e.manager+"/connect/github")
	require.Equal(t, http.StatusOK, status, page)
	assert.Contains(t, page, "github connected")
	assert.Equal(t, map[string]bool{"github": true, "code": false}, e.connected(t, "alice"))

	// The link connect_provider hands out completes in the browser too.
	link, _, err := e.conn.AuthorizationURL(ctx, "alice", "code")
	require.NoError(t, err)
	status, page = get(t, alice, link)
	require.Equal(t, http.StatusOK, status, page)
	assert.Equal(t, map[string]bool{"github": true, "code": true}, e.connected(t, "alice"))
	assert.Equal(t, map[string]bool{"github": false, "code": false}, e.connected(t, "bob"), "a sign-in is the person's own")

	// Each instance holds its own provider's token, sealed at rest.
	ghToken, err := e.store.AccessToken(ctx, "alice", "github")
	require.NoError(t, err)
	codeToken, err := e.store.AccessToken(ctx, "alice", "code")
	require.NoError(t, err)
	assert.True(t, e.github.Live(ghToken) && !e.code.Live(ghToken))
	assert.True(t, e.code.Live(codeToken) && !e.github.Live(codeToken))
	sealed := e.signIns(t)
	require.Len(t, sealed, 2)
	for _, s := range sealed {
		assert.False(t, bytes.Contains(s, []byte(ghToken)) || bytes.Contains(s, []byte(codeToken)), "a token is stored in clear")
	}

	// Disconnecting GitHub revokes the App's grant and forgets it alone.
	revoked, err := e.conn.Disconnect(ctx, "alice", "github")
	require.NoError(t, err)
	assert.True(t, revoked)
	assert.Equal(t, []connecttest.Revocation{{Method: "grant", Token: ghToken}}, e.github.Revocations())
	assert.False(t, e.github.Live(ghToken))
	assert.Equal(t, map[string]bool{"github": false, "code": true}, e.connected(t, "alice"))

	// The fake kind revokes by RFC 7009, the refresh token.
	revoked, err = e.conn.Disconnect(ctx, "alice", "code")
	require.NoError(t, err)
	assert.True(t, revoked)
	require.Len(t, e.code.Revocations(), 1)
	assert.Equal(t, "rfc7009", e.code.Revocations()[0].Method)
	assert.True(t, strings.HasPrefix(e.code.Revocations()[0].Token, "refresh-"))
	assert.Empty(t, e.signIns(t))

	// Disconnecting what is not connected is no error and revokes nothing.
	revoked, err = e.conn.Disconnect(ctx, "alice", "code")
	require.NoError(t, err)
	assert.False(t, revoked)
}

func TestCallbackRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("forged state", func(t *testing.T) {
		e := newConnectEnv(t)
		alice := connecttest.NewBrowser(t, e.dex, "alice")
		status, _ := get(t, alice, e.manager+"/callback/github?code=x&state=Zm9yZ2Vk")
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Empty(t, e.signIns(t))
	})

	t.Run("expired state", func(t *testing.T) {
		e := newConnectEnv(t)
		alice := connecttest.NewBrowser(t, e.dex, "alice")
		link, _, err := e.conn.AuthorizationURL(ctx, "alice", "github")
		require.NoError(t, err)
		e.skew.Store(int64(connect.DefaultStateTTL + time.Second))
		status, _ := get(t, alice, link)
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Empty(t, e.signIns(t))
	})

	t.Run("state of another instance", func(t *testing.T) {
		e := newConnectEnv(t)
		alice := connecttest.NewBrowser(t, e.dex, "alice")
		link, _, err := e.conn.AuthorizationURL(ctx, "alice", "github")
		require.NoError(t, err)
		cb := e.authorize(t, link)
		cb.Path = "/callback/code"
		status, _ := get(t, alice, cb.String())
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Empty(t, e.signIns(t))
	})

	t.Run("another person's state", func(t *testing.T) {
		e := newConnectEnv(t)
		// alice's link, completed in bob's browser: bob's grant must not
		// land in alice's sign-in.
		link, _, err := e.conn.AuthorizationURL(ctx, "alice", "github")
		require.NoError(t, err)
		bob := connecttest.NewBrowser(t, e.dex, "bob")
		status, page := get(t, bob, link)
		assert.Equal(t, http.StatusForbidden, status, page)
		assert.Contains(t, page, "another person")
		assert.Empty(t, e.signIns(t))
	})

	t.Run("wrong verifier", func(t *testing.T) {
		e := newConnectEnv(t)
		alice := connecttest.NewBrowser(t, e.dex, "alice")
		first, _, err := e.conn.AuthorizationURL(ctx, "alice", "github")
		require.NoError(t, err)
		second, _, err := e.conn.AuthorizationURL(ctx, "alice", "github")
		require.NoError(t, err)
		// The first flow's code under the second's state, whose verifier
		// does not match the code's challenge: an injected code.
		cb := e.authorize(t, first)
		secondState, err := url.Parse(second)
		require.NoError(t, err)
		q := cb.Query()
		q.Set("state", secondState.Query().Get("state"))
		cb.RawQuery = q.Encode()
		status, _ := get(t, alice, cb.String())
		assert.Equal(t, http.StatusBadGateway, status)
		assert.Empty(t, e.signIns(t))
	})

	t.Run("provider denied", func(t *testing.T) {
		e := newConnectEnv(t)
		alice := connecttest.NewBrowser(t, e.dex, "alice")
		status, _ := get(t, alice, e.manager+"/callback/github?error=access_denied")
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Empty(t, e.signIns(t))
	})

	t.Run("unknown provider", func(t *testing.T) {
		e := newConnectEnv(t)
		alice := connecttest.NewBrowser(t, e.dex, "alice")
		status, _ := get(t, alice, e.manager+"/connect/gitlab")
		assert.Equal(t, http.StatusNotFound, status)
	})
}
