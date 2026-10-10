package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/workspace-manager/internal/connect"
	"github.com/giantswarm/workspace-manager/internal/identity"
)

// ProvidersResult is list_providers' result.
type ProvidersResult struct {
	Providers []connect.Status `json:"providers"`
}

// ConnectResult is connect_provider's result.
type ConnectResult struct {
	Provider         string    `json:"provider"`
	AuthorizationURL string    `json:"authorizationURL"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// DisconnectResult is disconnect_provider's result.
type DisconnectResult struct {
	Provider string `json:"provider"`
	// Revoked is false when there was nothing left to revoke at the
	// provider: no sign-in, or one that could no longer be refreshed.
	Revoked bool `json:"revoked"`
}

// addProviderTools registers the person's provider tools.
func addProviderTools(s *mcpserver.MCPServer, c *connect.Connector) {
	providerArg := mcp.WithString("provider", mcp.Required(), mcp.Description("The provider instance's name, as list_providers shows it"))

	s.AddTool(mcp.NewTool("list_providers",
		mcp.WithDescription("List the installation's workspace providers, each with its kind, whether you are connected to it, and its connect link."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithOutputSchema[ProvidersResult](),
	), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		person, err := caller(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		list, err := c.Providers(ctx, person)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("list providers", err), nil
		}
		return mcp.NewToolResultStructured(ProvidersResult{Providers: list}, fmt.Sprintf("%d providers", len(list))), nil
	})

	s.AddTool(mcp.NewTool("connect_provider",
		mcp.WithDescription("Start connecting your account at a provider: returns the provider's sign-in link. Open it in your browser and approve; the link is yours alone and expires in minutes."),
		providerArg,
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithOutputSchema[ConnectResult](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		person, err := caller(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		name, err := req.RequireString("provider")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		u, expiry, err := c.AuthorizationURL(ctx, person, name)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("connect provider", err), nil
		}
		slog.InfoContext(ctx, "provider sign-in started", identity.LogAttr(ctx), "provider", name)
		return mcp.NewToolResultStructured(ConnectResult{Provider: name, AuthorizationURL: u, ExpiresAt: expiry.UTC()},
			"Open this link in your browser to connect "+name+": "+u), nil
	})

	s.AddTool(mcp.NewTool("disconnect_provider",
		mcp.WithDescription("Disconnect your account at a provider: revokes the grant at the provider and forgets your sign-in. Workspaces on the provider stop working for you until you connect again."),
		providerArg,
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithOutputSchema[DisconnectResult](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		person, err := caller(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		name, err := req.RequireString("provider")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		revoked, err := c.Disconnect(ctx, person, name)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("disconnect provider", err), nil
		}
		slog.InfoContext(ctx, "provider disconnected", identity.LogAttr(ctx), "provider", name, "revoked", revoked)
		return mcp.NewToolResultStructured(DisconnectResult{Provider: name, Revoked: revoked}, name+" disconnected"), nil
	})
}

// caller is the person a tool acts for: the caller's Dex subject.
func caller(ctx context.Context) (string, error) {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Subject == "" {
		return "", errors.New("no caller: provider sign-ins need the server's OAuth")
	}
	return id.Subject, nil
}
