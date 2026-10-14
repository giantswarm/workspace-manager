package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/authorize"
	"github.com/giantswarm/workspace-manager/internal/grant"
	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/signin"
)

type oneWorkspace struct{ ws *v1alpha1.Workspace }

func (o oneWorkspace) Get(context.Context, string) (*v1alpha1.Workspace, error) { return o.ws, nil }

func (o oneWorkspace) RequestSync(context.Context, string, time.Time) (*v1alpha1.Workspace, error) {
	return o.ws, nil
}

type noSignIns struct{}

func (noSignIns) Access(context.Context, string, string) (signin.Access, error) {
	return signin.Access{}, signin.ErrNotSignedIn
}

func TestAuthorizeWorkspaceTool(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	cs := k8sfake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keys", Namespace: "wm"},
		Data: map[string][]byte{"grant": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}})
	a, err := authorize.New(authorize.Config{
		Workspaces: oneWorkspace{&v1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "otter"}}},
		Core:       cs,
		Namespace:  "kagent",
		Tokens:     noSignIns{},
		ConnectURL: func(string) string { return "" },
		Signer:     &grant.Signer{Keys: &grant.KeySource{Client: cs, Namespace: "wm", Secret: "keys", Key: "grant"}, Issuer: "https://wm.example.com", Audience: "kagent"},
		Broker:     "workspace-manager",
	})
	require.NoError(t, err)
	cfg := Config{Authorizer: a}
	ctx := identity.ContextWith(context.Background(), &identity.Identity{Subject: "alice-sub", Email: "alice@example.com"})

	t.Run("listed", func(t *testing.T) {
		out := NewMCPServer(cfg, "test").HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)).(mcp.JSONRPCResponse)
		tools := out.Result.(mcp.ListToolsResult).Tools
		require.Len(t, tools, 1)
		assert.Equal(t, "authorize_workspace", tools[0].Name)
	})

	t.Run("no caller", func(t *testing.T) {
		res := callTool(t, context.Background(), cfg, "authorize_workspace", map[string]any{"workspace": "otter"})
		assert.True(t, res.IsError)
	})

	t.Run("a caller without an email gets no grant", func(t *testing.T) {
		noEmail := identity.ContextWith(context.Background(), &identity.Identity{Subject: "alice-sub"})
		res := callTool(t, noEmail, cfg, "authorize_workspace", map[string]any{"workspace": "otter"})
		require.True(t, res.IsError)
		require.Len(t, res.Content, 1)
		assert.Contains(t, res.Content[0].(mcp.TextContent).Text, authorize.ErrNoEmail.Error())
	})

	t.Run("a refusal is an answer, with its reason", func(t *testing.T) {
		res := callTool(t, ctx, cfg, "authorize_workspace", map[string]any{"workspace": "otter"})
		require.False(t, res.IsError)
		r := res.StructuredContent.(authorize.Result)
		assert.Equal(t, authorize.Refused, r.Outcome)
		assert.Contains(t, r.Reason, "has not synced yet")
		assert.Empty(t, r.Grant)
	})
}
