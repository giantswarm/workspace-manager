package server

import (
	"net/http"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/giantswarm/workspace-manager/internal/grant"
)

// routes registers every route of the listener: this file is the one place a
// new endpoint is added.
func (s *Server) routes(mux *http.ServeMux, cfg Config, mcpSrv *mcpserver.MCPServer) {
	mux.HandleFunc("GET /healthz", ok)
	// Readiness does not track the API server or Dex: the endpoint must stay
	// reachable so clients read a failure from the tool result instead of an
	// unready Service. It holds a starting replica back until its providers
	// answered once, and turns a stopping one away while it drains.
	mux.HandleFunc("GET /readyz", s.readyz)

	if s.oauth != nil {
		s.oauth.register(mux)
	}

	// The grant's verification keys are public: kagent's admission fetches
	// them without credentials.
	if cfg.JWKS != nil {
		mux.Handle("GET "+grant.JWKSPath, cfg.JWKS)
	}

	// mcp-go's default session manager issues session IDs without keeping
	// them, so any replica answers any request of a session. mcp-go reads a
	// POST body whole before validating it (CVE-2026-108859): the bound keeps
	// that read small.
	mcpHandler := mcpserver.NewStreamableHTTPServer(mcpSrv, mcpserver.WithEndpointPath(cfg.MCPPath))
	mux.Handle(cfg.MCPPath, http.MaxBytesHandler(s.guard(mcpHandler), maxMCPBodyBytes))
}

// maxMCPBodyBytes bounds an MCP request body.
const maxMCPBodyBytes = 1 << 20
