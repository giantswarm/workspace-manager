package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/internal/connect"
	"github.com/giantswarm/workspace-manager/internal/connect/connecttest"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
	"github.com/giantswarm/workspace-manager/internal/signin"
)

// oauthKind is a provider kind of nothing but an RFC 7009 sign-in.
type oauthKind struct{ base string }

func (k oauthKind) List(context.Context, oauth2.TokenSource, string) ([]provider.Item, error) {
	return nil, nil
}

func (k oauthKind) SyncCredential(context.Context, provider.Secrets, string) (oauth2.TokenSource, error) {
	return nil, nil
}

func (k oauthKind) SignIn() provider.SignIn {
	return provider.SignIn{AuthURL: k.base + "/oauth/authorize", TokenURL: k.base + "/oauth/token", RevocationURL: k.base + "/oauth/revoke",
		ClientID: "client", ClientSecret: provider.SecretRef{Name: "oauth", Key: "secret"}}
}

func (k oauthKind) Hosts() provider.Hosts { return provider.Hosts{} }

func (k oauthKind) SecretRefs() []provider.SecretRef {
	return []provider.SecretRef{{Name: "oauth", Key: "secret"}}
}

func callTool(t *testing.T, ctx context.Context, cfg Config, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	msg, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	require.NoError(t, err)
	resp := NewMCPServer(cfg, "test").HandleMessage(ctx, msg)
	out, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok, "response %#v", resp)
	res, ok := out.Result.(*mcp.CallToolResult)
	require.True(t, ok, "result %#v", out.Result)
	return res
}

func TestProviderTools(t *testing.T) {
	as := connecttest.NewAuthServer(t, "client", "client-secret")
	instances := []provider.Instance{{Name: "code", KindName: "oauth", Kind: oauthKind{base: as.URL}}}
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	keyring, err := signin.NewKeyring(map[string][]byte{"k": key}, "k")
	require.NoError(t, err)
	clients, err := connect.NewClients(instances, providertest.Secrets{{Name: "oauth", Key: "secret"}: []byte("client-secret")}, "https://wm.example.com")
	require.NoError(t, err)
	store, err := signin.NewKubeStore(signin.Options{Client: k8sfake.NewClientset(), Namespace: "wm", Keyring: keyring, OAuth2: clients, Identity: "test"})
	require.NoError(t, err)
	conn, err := connect.New(connect.Options{Clients: clients, Store: store, Keyring: keyring})
	require.NoError(t, err)
	cfg := Config{Providers: instances, Connector: conn}
	ctx := identity.ContextWith(context.Background(), &identity.Identity{Subject: "alice-sub", Email: "alice@example.com"})

	t.Run("no caller", func(t *testing.T) {
		res := callTool(t, context.Background(), cfg, "list_providers", nil)
		assert.True(t, res.IsError)
	})

	t.Run("list", func(t *testing.T) {
		res := callTool(t, ctx, cfg, "list_providers", nil)
		require.False(t, res.IsError)
		assert.Equal(t, ProvidersResult{Providers: []connect.Status{{Name: "code", Kind: "oauth", ConnectURL: "https://wm.example.com/connect/code"}}}, res.StructuredContent)
	})

	t.Run("connect", func(t *testing.T) {
		res := callTool(t, ctx, cfg, "connect_provider", map[string]any{"provider": "code"})
		require.False(t, res.IsError)
		r := res.StructuredContent.(ConnectResult)
		u, err := url.Parse(r.AuthorizationURL)
		require.NoError(t, err)
		q := u.Query()
		assert.Equal(t, as.URL+"/oauth/authorize", u.Scheme+"://"+u.Host+u.Path)
		assert.Equal(t, "https://wm.example.com/callback/code", q.Get("redirect_uri"))
		assert.Equal(t, "S256", q.Get("code_challenge_method"))
		assert.NotEmpty(t, q.Get("code_challenge"))
		person, err := conn.CheckState("code", q.Get("state"))
		require.NoError(t, err)
		assert.Equal(t, "alice-sub", person)
		assert.NotContains(t, r.AuthorizationURL, "code_verifier")
	})

	t.Run("unknown provider", func(t *testing.T) {
		res := callTool(t, ctx, cfg, "connect_provider", map[string]any{"provider": "gitlab"})
		assert.True(t, res.IsError)
	})

	t.Run("disconnect without a sign-in", func(t *testing.T) {
		res := callTool(t, ctx, cfg, "disconnect_provider", map[string]any{"provider": "code"})
		require.False(t, res.IsError)
		assert.Equal(t, DisconnectResult{Provider: "code"}, res.StructuredContent)
		assert.Empty(t, as.Revocations())
	})

	t.Run("no tools without a connector", func(t *testing.T) {
		msg := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		out := NewMCPServer(Config{}, "test").HandleMessage(ctx, msg).(mcp.JSONRPCResponse)
		assert.Empty(t, out.Result.(mcp.ListToolsResult).Tools)
	})
}
