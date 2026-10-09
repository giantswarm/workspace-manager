// Package api is workspace-manager's MCP surface. The server carries no tools
// yet; each workspace operation registers its tool here and reaches
// Kubernetes through Config.Kube, as the caller.
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

	"github.com/giantswarm/workspace-manager/internal/kube"
)

const tracerName = "github.com/giantswarm/workspace-manager"

// Config is what the tools work with.
type Config struct {
	// Kube hands out the caller's Kubernetes clients.
	Kube kube.Provider
	// Namespace is where the workspaces, their volumes, snapshots and session
	// grants live.
	Namespace string
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
func NewMCPServer(_ Config, version string) *mcpserver.MCPServer {
	return mcpserver.NewMCPServer("workspace-manager", version,
		mcpserver.WithToolHandlerMiddleware(genAIToolName),
		mcpserver.WithToolHandlerMiddleware(toolDuration(otel.Meter(tracerName))),
		// The HTTP server span already joined the inbound traceparent, so the
		// MCP spans nest under it instead of extracting it a second time.
		mcpotel.WithServerTracingPropagator(otel.Tracer(tracerName), propagation.NewCompositeTextMapPropagator()),
		mcpserver.WithToolCapabilities(false),
		mcpserver.WithInstructions("Manage the Agent Platform's workspaces: the sources an agent session works on. Every call acts as the caller."),
	)
}
