package server

import (
	"net/http"

	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Pages are the person's browser pages for connecting a provider
// (connect.Pages).
type Pages interface {
	Connect(w http.ResponseWriter, r *http.Request)
	Callback(w http.ResponseWriter, r *http.Request)
	SignIn(w http.ResponseWriter, r *http.Request)
}

// routes registers every route of the listener: this file is the one place a
// new endpoint is added.
func (s *Server) routes(mux *http.ServeMux, cfg Config, mcpSrv *mcpserver.MCPServer) {
	mux.HandleFunc("GET /healthz", ok)
	// Readiness does not track the API server or Dex: the endpoint must stay
	// reachable so clients read a failure from the tool result instead of an
	// unready Service.
	mux.HandleFunc("GET /readyz", ok)

	if s.oauth != nil {
		s.oauth.register(mux)
	}

	// The browser's pages authenticate the person themselves (a Dex sign-in
	// of their own), never with a bearer token.
	if cfg.Pages != nil {
		mux.HandleFunc("GET /connect/{instance}", cfg.Pages.Connect)
		mux.HandleFunc("GET /callback/{instance}", cfg.Pages.Callback)
		mux.HandleFunc("GET /signin", cfg.Pages.SignIn)
	}

	// The token exchange authenticates its client itself (client_secret_basic
	// or client_secret_post) and validates the subject token in the form,
	// never a bearer token.
	if s.exchange != nil {
		s.exchange.Register(mux)
	}

	// mcp-go's default session manager issues session IDs without keeping
	// them, so any replica answers any request of a session.
	mux.Handle(cfg.MCPPath, s.guard(mcpserver.NewStreamableHTTPServer(mcpSrv, mcpserver.WithEndpointPath(cfg.MCPPath))))
}
