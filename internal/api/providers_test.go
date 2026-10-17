package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

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

// TestListProviders: every instance, connected exactly when muster attached
// the person's App token to the call, the pinned one alone; the person's
// sign-in tools are gone.
func TestListProviders(t *testing.T) {
	cfg := Config{
		Providers: []provider.Instance{{Name: "github", KindName: "github"}, {Name: "gitlab", KindName: "gitlab"}},
		Grants:    muster.Grants{Instance: "github", Server: "workspace-manager"},
	}
	alice := identity.ContextWith(context.Background(), &identity.Identity{Subject: "alice-sub", Email: "alice@example.com"})

	t.Run("listed alone", func(t *testing.T) {
		out := NewMCPServer(cfg, "test").HandleMessage(alice, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)).(mcp.JSONRPCResponse)
		var names []string
		for _, tool := range out.Result.(mcp.ListToolsResult).Tools {
			names = append(names, tool.Name)
		}
		assert.Equal(t, []string{"list_providers"}, names, "no connect_provider, no disconnect_provider")
	})

	t.Run("no caller", func(t *testing.T) {
		assert.True(t, callTool(t, context.Background(), cfg, "list_providers", nil).IsError)
	})

	list := func(ctx context.Context) []ProviderStatus {
		res := callTool(t, ctx, cfg, "list_providers", nil)
		require.False(t, res.IsError)
		return res.StructuredContent.(ProvidersResult).Providers
	}

	t.Run("granted in muster", func(t *testing.T) {
		got := list(identity.ContextWithProviderToken(alice, "github", "ghu_alice"))
		require.Len(t, got, 2)
		assert.Equal(t, ProviderStatus{Name: "github", Kind: "github", Connected: true}, got[0])
		assert.False(t, got[1].Connected)
		assert.Contains(t, got[1].SignIn, "muster holds no grant for provider gitlab")
	})

	t.Run("no token on the call", func(t *testing.T) {
		got := list(alice)
		require.Len(t, got, 2)
		assert.False(t, got[0].Connected)
		assert.Contains(t, got[0].SignIn, "core_auth_login server=workspace-manager")
	})
}
