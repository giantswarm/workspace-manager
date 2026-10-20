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
// the person's App token to the call, each App's on its own; the person's
// sign-in tools are gone.
func TestListProviders(t *testing.T) {
	cfg := Config{
		Providers: []provider.Instance{{Name: "github", KindName: "github"}, {Name: "gitlab", KindName: "gitlab"}},
		Grants:    muster.Grants{Apps: []string{"github", "gitlab"}, Server: "workspace-manager"},
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

	t.Run("both granted in muster", func(t *testing.T) {
		got := list(identity.ContextWithProviderToken(identity.ContextWithProviderToken(alice, "github", "ghu_alice"), "gitlab", "glpat_alice"))
		assert.Equal(t, []ProviderStatus{{Name: "github", Kind: "github", Connected: true}, {Name: "gitlab", Kind: "gitlab", Connected: true}}, got)
	})

	t.Run("one granted in muster", func(t *testing.T) {
		got := list(identity.ContextWithProviderToken(alice, "github", "ghu_alice"))
		require.Len(t, got, 2)
		assert.Equal(t, ProviderStatus{Name: "github", Kind: "github", Connected: true}, got[0])
		assert.False(t, got[1].Connected)
		assert.Equal(t, "grant the App of provider gitlab once in muster: core_auth_login app=gitlab", got[1].SignIn)
	})

	t.Run("pinned registration", func(t *testing.T) {
		pinned := cfg
		pinned.Grants = muster.Grants{Instance: "github", Server: "workspace-manager"}
		res := callTool(t, alice, pinned, "list_providers", nil)
		require.False(t, res.IsError)
		got := res.StructuredContent.(ProvidersResult).Providers
		require.Len(t, got, 2)
		assert.Contains(t, got[0].SignIn, "core_auth_login server=workspace-manager")
		assert.Contains(t, got[1].SignIn, "muster holds no grant for provider gitlab")
	})
}
