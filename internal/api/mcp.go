// Package api is workspace-manager's MCP surface: the person's provider
// sign-ins (providers.go) and the workspace operations' tools, each reading
// and writing through a workspace.Store, which checks the caller's
// Organization and writes with Config.Kube, the manager's own clients.
package api

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	mcpotel "github.com/mark3labs/mcp-go/otel"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/giantswarm/workspace-manager/internal/connect"
	"github.com/giantswarm/workspace-manager/internal/kube"
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
	// Connector, when set, serves the person's provider sign-ins:
	// list_providers, connect_provider and disconnect_provider.
	Connector *connect.Connector
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
	if cfg.Connector != nil {
		addProviderTools(s, cfg.Connector)
	}
	return s
}
