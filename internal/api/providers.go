package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/workspace-manager/internal/identity"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

// ProviderStatus is one provider instance and whether the person granted its
// App in muster.
type ProviderStatus struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Connected bool   `json:"connected"`
	// SignIn says how the person grants the App, while they have not.
	SignIn string `json:"signIn,omitempty"`
}

// ProvidersResult is list_providers' result.
type ProvidersResult struct {
	Providers []ProviderStatus `json:"providers"`
}

// addProviderTools registers list_providers.
func addProviderTools(s *mcpserver.MCPServer, instances []provider.Instance, grants muster.Grants) {
	s.AddTool(mcp.NewTool("list_providers",
		mcp.WithDescription("List the installation's workspace providers, each with its kind and whether you granted its App in muster, the one place your provider sign-in lives."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithOutputSchema[ProvidersResult](),
	), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if _, err := callerIdentity(ctx); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		list := make([]ProviderStatus, 0, len(instances))
		for _, in := range instances {
			st := ProviderStatus{Name: in.Name, Kind: in.KindName, Connected: grants.Connected(ctx, in.Name)}
			if !st.Connected {
				st.SignIn = grants.SignIn(in.Name)
			}
			list = append(list, st)
		}
		return mcp.NewToolResultStructured(ProvidersResult{Providers: list}, fmt.Sprintf("%d providers", len(list))), nil
	})
}

// callerIdentity is the authenticated caller, without whom no tool acts.
func callerIdentity(ctx context.Context) (*identity.Identity, error) {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Subject == "" {
		return nil, errors.New("no caller: this tool needs the server's OAuth")
	}
	return id, nil
}
