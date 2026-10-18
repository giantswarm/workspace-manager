package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/workspace-manager/internal/authorize"
	"github.com/giantswarm/workspace-manager/internal/identity"
)

// addAuthorizeTool registers authorize_workspace.
func addAuthorizeTool(s *mcpserver.MCPServer, a *authorize.Authorizer) {
	s.AddTool(mcp.NewTool("authorize_workspace",
		tooldesc.Describe("Check that you can read every repository of a workspace before starting a Session on it: a grant, or why not.",
			"Granted: a short-lived grant to pass to start_session, naming the repositories pushed to since the last sync. "+
				"Refused: the repositories you cannot read, or why the workspace cannot be started on yet. "+
				"connect_required: the providers whose App to grant in muster first, and how."),
		mcp.WithString("workspace", mcp.Required(), mcp.Description("The workspace's name")),
		readOnly(),
		mcp.WithOpenWorldHintAnnotation(true),
		mcp.WithOutputSchema[authorize.Result](),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := callerIdentity(ctx)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		name, err := req.RequireString("workspace")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		res, err := a.Authorize(ctx, id, name)
		if err != nil {
			return mcp.NewToolResultErrorFromErr("authorize workspace", err), nil
		}
		slog.InfoContext(ctx, "workspace authorized", identity.LogAttr(ctx), "workspace", name, "outcome", res.Outcome,
			"repositories", len(res.Repositories), "changed", len(res.Changed), "unreadable", len(res.Unreadable), "connect", len(res.Connect))
		text := res.Reason
		if res.Outcome == authorize.Granted {
			text = fmt.Sprintf("Granted for workspace %s until %s: %d repositories, %d changed since the last sync.",
				name, res.ExpiresAt.Format("15:04:05 MST"), len(res.Repositories), len(res.Changed))
		}
		return mcp.NewToolResultStructured(*res, text), nil
	})
}
