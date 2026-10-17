// Package server assembles the single HTTP listener: health endpoints and the
// MCP streamable-HTTP endpoint behind the OAuth guard when configured, and
// behind muster's App pin before it when pinned (Pin). Without
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
	"sync/atomic"
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
	// Pin, when set, makes the MCP endpoint muster's registration pinned to
	// a provider instance's App: the bearer is the person's App token,
	// muster's grant, and the Dex ID token arrives in
	// ForwardedIdentityHeader. It needs OAuth, which validates that token.
	Pin *Pin
	// JWKS, when set, publishes the workspace grant's verification keys at
	// grant.JWKSPath.
	JWKS http.Handler
	// Ready, when set, holds /readyz back until it reports true: the
	// provider path answers (provider.Reach).
	Ready func() bool
	// Drain is how long the listener keeps serving after the stop signal,
	// /readyz refusing, before it shuts down: the time the Service takes to
	// stop sending new connections, which would otherwise be refused.
	Drain time.Duration
}

// Server is the assembled HTTP server.
type Server struct {
	http     *http.Server
	oauth    *oauthRuntime
	pin      *pinGuard
	ready    func() bool
	drain    time.Duration
	draining atomic.Bool
	log      *slog.Logger
}

// New builds the server.
func New(cfg Config, mcpSrv *mcpserver.MCPServer, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MCPPath == "" {
		cfg.MCPPath = "/mcp"
	}
	s := &Server{log: log, ready: cfg.Ready, drain: cfg.Drain}
	if cfg.OAuth != nil {
		o, err := newOAuth(*cfg.OAuth, cfg.MCPPath, log)
		if err != nil {
			return nil, err
		}
		s.oauth = o
	}
	if cfg.Pin != nil {
		if s.oauth == nil {
			return nil, errors.New("muster pin: needs OAuth, which validates the forwarded ID token")
		}
		p, err := newPinGuard(*cfg.Pin, log)
		if err != nil {
			return nil, err
		}
		s.pin = p
	}
	mux := http.NewServeMux()
	s.routes(mux, cfg, mcpSrv)
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           traced(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: MCP streams outlive any fixed value.
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
}

// readyz answers ready once the provider path answers (Config.Ready), and
// not ready while draining.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	switch {
	case s.draining.Load():
		http.Error(w, "draining", http.StatusServiceUnavailable)
	case s.ready != nil && !s.ready():
		http.Error(w, "waiting for the providers to answer", http.StatusServiceUnavailable)
	default:
		ok(w, r)
	}
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

// guard requires an authenticated caller when OAuth is on, and the person's
// App token muster attaches when pinned.
func (s *Server) guard(next http.Handler) http.Handler {
	if s.oauth == nil {
		return next
	}
	if s.pin != nil {
		return s.pin.protect(s.oauth.protect(next))
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
	if s.drain > 0 {
		s.draining.Store(true)
		s.log.Info("draining: not ready, still serving", "for", s.drain)
		select {
		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("http server: %w", err)
			}
			return nil
		case <-time.After(s.drain):
		}
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
