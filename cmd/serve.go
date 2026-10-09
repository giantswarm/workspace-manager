package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/giantswarm/mcp-toolkit/metrics"
	"github.com/giantswarm/mcp-toolkit/tracing"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/kube"
	"github.com/giantswarm/workspace-manager/internal/server"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

// kubeFlags select how a command reaches the API server.
type kubeFlags struct {
	kubeconfig string
	context    string
	inCluster  bool
}

func (k *kubeFlags) add(f *pflag.FlagSet) {
	f.StringVar(&k.kubeconfig, "kubeconfig", envOr("KUBECONFIG", ""), "Kubeconfig path; empty uses the default loading rules or in-cluster auth (KUBECONFIG)")
	f.StringVar(&k.context, "kube-context", envOr("KUBE_CONTEXT", ""), "Kubeconfig context override (KUBE_CONTEXT)")
	f.BoolVar(&k.inCluster, "in-cluster", envBool("KUBERNETES_IN_CLUSTER", false), "Force in-cluster Kubernetes auth (KUBERNETES_IN_CLUSTER)")
}

func (k *kubeFlags) config() kube.Config {
	return kube.Config{Kubeconfig: k.kubeconfig, Context: k.context, InCluster: k.inCluster}
}

type serveOptions struct {
	listen string

	kube kubeFlags

	namespace     string
	organizations []string

	mcpPath string

	oauthEnabled                  bool
	oauthBaseURL                  string
	dexIssuerURL                  string
	dexClientID                   string
	dexClientSecret               string
	dexCAFile                     string
	dexAllowPrivateIP             bool
	oauthTrustedAudiences         string
	ssoAllowPrivateIPs            bool
	allowPublicClientRegistration bool
}

func newServeCmd() *cobra.Command {
	o := &serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server",
		Long: `Run the workspace-manager server. Every flag can also be set through the
environment variable named next to it; flags win over the environment.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServe(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", envOr("WORKSPACE_MANAGER_LISTEN", ":8080"), "Listen address (WORKSPACE_MANAGER_LISTEN)")
	o.kube.add(f)
	f.StringVar(&o.namespace, "namespace", envOr("WORKSPACE_MANAGER_NAMESPACE", "kagent"), "Namespace the workspaces and everything they own live in (WORKSPACE_MANAGER_NAMESPACE)")
	f.StringArrayVar(&o.organizations, "organization", nil, "An Organization and its member groups, `<organization>=<group>[,<group>...]`, once per Organization: a caller carrying any of the groups reads and writes the Organization's workspaces; an Organization not given has no members")
	f.StringVar(&o.mcpPath, "mcp-path", envOr("WORKSPACE_MANAGER_MCP_PATH", "/mcp"), "MCP endpoint path (WORKSPACE_MANAGER_MCP_PATH)")
	f.BoolVar(&o.oauthEnabled, "enable-oauth", envBool("WORKSPACE_MANAGER_OAUTH_ENABLED", false), "Require an OAuth 2.1 bearer token on the MCP endpoint, validated against Dex (mcp-oauth); the caller's identity and Dex token travel with every request, and a request without a Dex token is refused (WORKSPACE_MANAGER_OAUTH_ENABLED)")
	f.StringVar(&o.oauthBaseURL, "oauth-base-url", envOr("WORKSPACE_MANAGER_OAUTH_BASE_URL", ""), "Public base URL of this server: the issuer of its OAuth metadata, https or loopback http (WORKSPACE_MANAGER_OAUTH_BASE_URL)")
	f.StringVar(&o.dexIssuerURL, "dex-issuer-url", envOr("DEX_ISSUER_URL", ""), "Dex issuer URL (DEX_ISSUER_URL)")
	f.StringVar(&o.dexClientID, "dex-client-id", envOr("DEX_CLIENT_ID", ""), "Dex client ID (DEX_CLIENT_ID)")
	f.StringVar(&o.dexClientSecret, "dex-client-secret", envOr("DEX_CLIENT_SECRET", ""), "Dex client secret; prefer the environment (DEX_CLIENT_SECRET)")
	f.StringVar(&o.dexCAFile, "dex-ca-file", envOr("DEX_CA_FILE", ""), "PEM CA bundle of a Dex with a private certificate; verifies discovery, token and JWKS calls (DEX_CA_FILE)")
	f.BoolVar(&o.dexAllowPrivateIP, "allow-private-oauth-urls", envBool("WORKSPACE_MANAGER_OAUTH_ALLOW_PRIVATE_URLS", false), "Let the Dex issuer resolve to a private or loopback address, an in-cluster Dex (WORKSPACE_MANAGER_OAUTH_ALLOW_PRIVATE_URLS)")
	f.StringVar(&o.oauthTrustedAudiences, "oauth-trusted-audiences", envOr("OAUTH_TRUSTED_AUDIENCES", ""), "Comma-separated OAuth client IDs whose Dex id_tokens are accepted as bearer tokens: the platform client and the audiences muster requires, which every forwarded token carries (OAUTH_TRUSTED_AUDIENCES)")
	f.BoolVar(&o.ssoAllowPrivateIPs, "sso-allow-private-ips", envBool("SSO_ALLOW_PRIVATE_IPS", false), "Let Dex's JWKS endpoint resolve to a private address when validating forwarded tokens (SSO_ALLOW_PRIVATE_IPS)")
	f.BoolVar(&o.allowPublicClientRegistration, "allow-public-client-registration", envBool("WORKSPACE_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION", false), "Accept unauthenticated dynamic client registration; labs only (WORKSPACE_MANAGER_OAUTH_ALLOW_PUBLIC_REGISTRATION)")
	return cmd
}

func runServe(ctx context.Context, o *serveOptions) error {
	log := slog.Default()

	// Exports only when OTEL_EXPORTER_OTLP_ENDPOINT (or OTEL_TRACES_EXPORTER)
	// is set; the W3C propagator is installed either way.
	shutdownTracing, err := tracing.Init(ctx, tracing.WithServiceName("workspace-manager"), tracing.WithServiceVersion(build.Version))
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer flush(ctx, "traces", shutdownTracing, log)
	// Serves Prometheus metrics on its own listener with
	// OTEL_METRICS_EXPORTER=prometheus (the chart's default).
	shutdownMetrics, err := metrics.Init(ctx, metrics.WithServiceName("workspace-manager"), metrics.WithServiceVersion(build.Version))
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}
	defer flush(ctx, "metrics", shutdownMetrics, log)

	orgs, err := workspace.ParseOrganizations(o.organizations)
	if err != nil {
		return err
	}

	// The manager's own ServiceAccount: it writes the workspace objects, the
	// volume and the jobs once the caller's Organization is checked.
	clients, err := kube.New(o.kube.config())
	if err != nil {
		return fmt.Errorf("workspace-manager needs Kubernetes access: %w", err)
	}

	srvCfg := server.Config{Addr: o.listen, MCPPath: o.mcpPath}
	if o.oauthEnabled {
		srvCfg.OAuth = &server.OAuthConfig{
			BaseURL:                       o.oauthBaseURL,
			DexIssuerURL:                  o.dexIssuerURL,
			DexClientID:                   o.dexClientID,
			DexClientSecret:               o.dexClientSecret,
			DexCAFile:                     o.dexCAFile,
			DexAllowPrivateIP:             o.dexAllowPrivateIP,
			TrustedAudiences:              splitList(o.oauthTrustedAudiences),
			SSOAllowPrivateIPs:            o.ssoAllowPrivateIPs,
			AllowPublicClientRegistration: o.allowPublicClientRegistration,
		}
	}
	srv, err := server.New(srvCfg, api.NewMCPServer(api.Config{Kube: clients, Namespace: o.namespace, Organizations: orgs}, build.Version), log)
	if err != nil {
		return err
	}
	log.Info("workspace-manager starting", "version", build.Version, "commit", build.Commit, "listen", o.listen,
		"mcp", o.mcpPath, "oauth", o.oauthEnabled, "namespace", o.namespace, "organizations", len(orgs))

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Run(ctx)
}

// flush runs an exporter's shutdown with a bounded timeout, after the server
// stopped.
func flush(ctx context.Context, what string, shutdown func(context.Context) error, log *slog.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := shutdown(shutdownCtx); err != nil {
		log.Warn("flushing "+what+" failed", "error", err)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
