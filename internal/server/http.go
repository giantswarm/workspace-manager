// Package server assembles the single HTTP listener: health endpoints and the
// MCP streamable-HTTP endpoint behind the OAuth guard when configured. Without
// OAuth there is no authentication and no caller: only a server nothing but a
// trusted proxy can reach runs that way, and every tool that needs the
// caller's Organization refuses.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Config configures the listener.
type Config struct {
	Addr    string
	MCPPath string
	// OAuth, when set, makes the server an OAuth 2.1 resource server: the MCP
	// endpoint requires a bearer token Dex issued (forwarded by muster) or
	// this server's own, and every call carries the caller's identity and
	// Dex token.
	OAuth *OAuthConfig
}

// Server is the assembled HTTP server.
type Server struct {
	http  *http.Server
	oauth *oauthRuntime
	log   *slog.Logger
}

// New builds the server.
func New(cfg Config, mcpSrv *mcpserver.MCPServer, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MCPPath == "" {
		cfg.MCPPath = "/mcp"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", ok)
	// Readiness does not track the API server or Dex: the endpoint must stay
	// reachable so clients read a failure from the tool result instead of an
	// unready Service.
	mux.HandleFunc("GET /readyz", ok)

	s := &Server{log: log}
	if cfg.OAuth != nil {
		o, err := newOAuth(*cfg.OAuth, cfg.MCPPath, log)
		if err != nil {
			return nil, err
		}
		o.register(mux)
		s.oauth = o
	}

	// mcp-go's default session manager issues session IDs without keeping
	// them, so any replica answers any request of a session.
	mux.Handle(cfg.MCPPath, s.guard(mcpserver.NewStreamableHTTPServer(mcpSrv, mcpserver.WithEndpointPath(cfg.MCPPath))))
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           traced(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: MCP streams outlive any fixed value.
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// traced opens a server span per request, joined to an inbound traceparent,
// named after the matched route so the name stays low-cardinality. Probes
// are not traced.
func traced(mux *http.ServeMux) http.Handler {
	return otelhttp.NewHandler(mux, "workspace-manager",
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			switch {
			case r.Pattern == "":
				return r.Method
			case strings.Contains(r.Pattern, " "):
				return r.Pattern
			default:
				return r.Method + " " + r.Pattern
			}
		}),
	)
}

// guard requires an authenticated caller when OAuth is on.
func (s *Server) guard(next http.Handler) http.Handler {
	if s.oauth == nil {
		return next
	}
	return s.oauth.protect(next)
}

// Handler exposes the mux (tests).
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run serves until ctx is done, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", s.http.Addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if s.oauth != nil {
		s.oauth.shutdown(shutdownCtx)
	}
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	s.log.Info("server stopped")
	return nil
}
