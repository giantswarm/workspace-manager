// Package api is workspace-manager's MCP surface: the person's provider
// sign-ins (providers.go), the access check at a Session's start
// (authorize.go), the workspaces' tools (workspaces.go) and the selection's
// (repositories.go), each reading and writing through a workspace.Store,
// which checks the caller's Organization and writes with Config.Kube, the
// manager's own clients.
package api

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpotel "github.com/mark3labs/mcp-go/otel"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/giantswarm/workspace-manager/internal/authorize"
	"github.com/giantswarm/workspace-manager/internal/controller"
	"github.com/giantswarm/workspace-manager/internal/kube"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

const tracerName = "github.com/giantswarm/workspace-manager"

// Config is what the tools work with.
type Config struct {
	// Kube is the manager's ServiceAccount clients.
	Kube kube.Client
	// Namespace is where the workspaces and everything they own live.
	Namespace string
	// Organizations decides who may read and write an Organization's
	// workspaces (workspace.Store checks it on every call).
	Organizations workspace.Organizations
	// Providers are the installation's provider instances.
	Providers []provider.Instance
	// Secrets read the provider instances' Secrets: with Providers, they
	// serve list_repositories and preview_workspace.
	Secrets provider.Secrets
	// Sizing is the installation's volume sizing, which preview_workspace
	// estimates a selection's volume with.
	Sizing controller.Sizing
	// Grants are the person's provider tokens muster attaches to the call:
	// with Providers, they serve list_providers.
	Grants muster.Grants
	// Authorizer, when set, serves authorize_workspace.
	Authorizer *authorize.Authorizer
}

// genAIToolName labels the mcp.tools/call server span with the called tool
// under the GenAI semantic-convention key.
func genAIToolName(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("gen_ai.tool.name", req.Params.Name))
		return next(ctx, req)
	}
}

// NewMCPServer builds the MCP server with its tracing and metrics middleware.
func NewMCPServer(cfg Config, version string) *mcpserver.MCPServer {
	s := mcpserver.NewMCPServer("workspace-manager", version,
		mcpserver.WithToolHandlerMiddleware(genAIToolName),
		mcpserver.WithToolHandlerMiddleware(toolDuration(otel.Meter(tracerName))),
		// The HTTP server span already joined the inbound traceparent, so the
		// MCP spans nest under it instead of extracting it a second time.
		mcpotel.WithServerTracingPropagator(otel.Tracer(tracerName), propagation.NewCompositeTextMapPropagator()),
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Manage the Agent Platform's workspaces: the sources an agent session works on. Every call is checked against the caller's Organization."),
	)
	if cfg.Kube != nil {
		names := make([]string, len(cfg.Providers))
		for i, p := range cfg.Providers {
			names[i] = p.Name
		}
		store := workspace.NewStore(cfg.Kube.Dynamic(), cfg.Namespace, cfg.Organizations, names)
		addWorkspaceTools(s, &workspaceTools{store: store, core: cfg.Kube.Typed(), namespace: cfg.Namespace, now: time.Now})
		if len(cfg.Providers) > 0 && cfg.Secrets != nil {
			sizing := cfg.Sizing
			if sizing == (controller.Sizing{}) {
				sizing = controller.DefaultSizing
			}
			addSelectionTools(s, &selectionTools{store: store, listings: workspace.NewListings(cfg.Providers, cfg.Secrets), sizing: sizing})
		}
	}
	if len(cfg.Providers) > 0 {
		addProviderTools(s, cfg.Providers, cfg.Grants)
	}
	if cfg.Authorizer != nil {
		addAuthorizeTool(s, cfg.Authorizer)
	}
	return s
}
