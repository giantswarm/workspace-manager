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
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/connect"
	"github.com/giantswarm/workspace-manager/internal/controller"
	"github.com/giantswarm/workspace-manager/internal/exchange"
	"github.com/giantswarm/workspace-manager/internal/kube"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/kinds"
	"github.com/giantswarm/workspace-manager/internal/server"
	"github.com/giantswarm/workspace-manager/internal/signin"
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
	storageClass  string

	syncCycle           time.Duration
	sessionCleanupAfter time.Duration
	sizingFactor        float64
	sizingHeadroom      string
	sizingMaxSize       string

	mcpPath string

	providersConfig string

	managerNamespace string
	signInKeysSecret string
	signInCurrentKey string
	grantSigningKey  string

	tokenExchangeClientID     string
	tokenExchangeClientSecret string

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
	f.StringVar(&o.storageClass, "storage-class", envOr("WORKSPACE_MANAGER_STORAGE_CLASS", ""), "The read-write-many StorageClass every workspace's volume is claimed from; empty claims none: each Workspace's VolumeClaimed condition names the missing value, and the cluster's default class is never used in its place (WORKSPACE_MANAGER_STORAGE_CLASS)")
	f.DurationVar(&o.syncCycle, "sync-cycle", envDuration("WORKSPACE_MANAGER_SYNC_CYCLE", controller.DefaultResync), "How often the controller reconciles every Workspace without an event, a Go duration (WORKSPACE_MANAGER_SYNC_CYCLE)")
	f.DurationVar(&o.sessionCleanupAfter, "sessions-cleanup-after", envDuration("WORKSPACE_MANAGER_SESSIONS_CLEANUP_AFTER", controller.DefaultSessionCleanupAfter), "How long a Session's directory outlives the Session's last turn, a Go duration (WORKSPACE_MANAGER_SESSIONS_CLEANUP_AFTER)")
	f.Float64Var(&o.sizingFactor, "sizing-factor", envFloat("WORKSPACE_MANAGER_SIZING_FACTOR", controller.DefaultSizing.Factor), "A new workspace volume's size is the measured need times this factor, at least 1, plus the headroom (WORKSPACE_MANAGER_SIZING_FACTOR)")
	f.StringVar(&o.sizingHeadroom, "sizing-headroom", envOr("WORKSPACE_MANAGER_SIZING_HEADROOM", controller.DefaultSizing.Headroom.String()), "Headroom added to a new workspace volume's multiplied need, a Kubernetes quantity; a Workspace's larger spec.sizing.headroom replaces it (WORKSPACE_MANAGER_SIZING_HEADROOM)")
	f.StringVar(&o.sizingMaxSize, "sizing-max-size", envOr("WORKSPACE_MANAGER_SIZING_MAX_SIZE", ""), "Ceiling of a new workspace volume's size, a Kubernetes quantity; empty sets none (WORKSPACE_MANAGER_SIZING_MAX_SIZE)")
	f.StringVar(&o.providersConfig, "providers-config", envOr("WORKSPACE_MANAGER_PROVIDERS_CONFIG", ""), "Provider instances file (YAML, the chart's `providers`); empty configures none (WORKSPACE_MANAGER_PROVIDERS_CONFIG)")
	f.StringVar(&o.managerNamespace, "manager-namespace", envOr("POD_NAMESPACE", ""), "The manager's own namespace: provider Secrets, the sealing keys and the sign-ins live there (POD_NAMESPACE)")
	f.StringVar(&o.signInKeysSecret, "signin-keys-secret", envOr("WORKSPACE_MANAGER_SIGNIN_KEYS_SECRET", ""), "Secret in the manager's namespace with the sign-in sealing keys, each data entry a key id and its 32 raw bytes; required with providers and OAuth (WORKSPACE_MANAGER_SIGNIN_KEYS_SECRET)")
	f.StringVar(&o.signInCurrentKey, "signin-current-key", envOr("WORKSPACE_MANAGER_SIGNIN_CURRENT_KEY", ""), "The id of the sealing key that seals new sign-ins (WORKSPACE_MANAGER_SIGNIN_CURRENT_KEY)")
	f.StringVar(&o.grantSigningKey, "grant-signing-key", envOr("WORKSPACE_MANAGER_GRANT_SIGNING_KEY", ""), "The key the workspace grant a Session carries is signed with, as `<secret>/<key>` of a Secret in the manager's namespace, at least 32 bytes, read at start; empty signs no grant. Needs providers and OAuth (WORKSPACE_MANAGER_GRANT_SIGNING_KEY)")
	f.StringVar(&o.tokenExchangeClientID, "token-exchange-client-id", envOr("WORKSPACE_MANAGER_TOKEN_EXCHANGE_CLIENT_ID", ""), "The installation's kagent client: the only client the token exchange `POST /token` (RFC 8693) answers; empty serves no token exchange. Needs providers and OAuth (WORKSPACE_MANAGER_TOKEN_EXCHANGE_CLIENT_ID)")
	f.StringVar(&o.tokenExchangeClientSecret, "token-exchange-client-secret", envOr("WORKSPACE_MANAGER_TOKEN_EXCHANGE_CLIENT_SECRET", ""), "The kagent client's secret as `<secret>/<key>` of a Secret in the manager's namespace, read on every exchange (WORKSPACE_MANAGER_TOKEN_EXCHANGE_CLIENT_SECRET)")
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

	// A configuration error fails the start, before anything else is built.
	providers, err := loadProviders(o.providersConfig)
	if err != nil {
		return err
	}
	for _, p := range providers {
		log.Info("provider configured", "name", p.Name, "kind", p.KindName)
	}
	orgs, err := workspace.ParseOrganizations(o.organizations)
	if err != nil {
		return err
	}

	ctlCfg, err := o.controllerConfig()
	if err != nil {
		return err
	}
	// The manager's own ServiceAccount: it writes the workspace objects, the
	// volume and the jobs once the caller's Organization is checked.
	clients, err := kube.New(o.kube.config())
	if err != nil {
		return fmt.Errorf("workspace-manager needs Kubernetes access: %w", err)
	}
	ctlCfg.Dynamic, ctlCfg.Core, ctlCfg.Logger = clients.Dynamic(), clients.Typed(), log
	// The controller claims each Workspace's volume; without a class it
	// claims none and says so in every Workspace's condition.
	if o.storageClass == "" {
		log.Warn("no StorageClass: no workspace volume is claimed until --storage-class (the chart's storage.storageClassName) names the installation's read-write-many class")
	}
	ctl := controller.New(ctlCfg)

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
	apiCfg := api.Config{Kube: clients, Namespace: o.namespace, Providers: providers, Organizations: orgs}
	if len(providers) > 0 {
		if srvCfg.OAuth == nil {
			log.Warn("provider sign-ins are off: they need --enable-oauth, which names the person")
		} else {
			si, err := providerSignIns(ctx, o, srvCfg.OAuth, clients, providers, log)
			if err != nil {
				return err
			}
			apiCfg.Connector, srvCfg.Pages = si.connector, si.pages
			if srvCfg.TokenExchange, err = tokenExchange(o, si, providers, log); err != nil {
				return err
			}
		}
	}
	if o.grantSigningKey != "" && apiCfg.Connector == nil {
		return fmt.Errorf("--grant-signing-key needs providers and --enable-oauth")
	}
	if o.tokenExchangeClientID != "" && srvCfg.TokenExchange == nil {
		return fmt.Errorf("--token-exchange-client-id needs providers and --enable-oauth")
	}
	srv, err := server.New(srvCfg, api.NewMCPServer(apiCfg, build.Version), log)
	if err != nil {
		return err
	}
	log.Info("workspace-manager starting", "version", build.Version, "commit", build.Commit, "listen", o.listen,
		"mcp", o.mcpPath, "oauth", o.oauthEnabled, "namespace", o.namespace, "organizations", len(orgs), "storageClass", o.storageClass)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The server and the controller stop together: on a signal, or when one
	// of them fails.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(ctx) })
	g.Go(func() error { return ctl.Run(ctx) })
	return g.Wait()
}

// signIns are the person's provider sign-ins as serve wires them.
type signIns struct {
	connector *connect.Connector
	pages     *connect.Pages
	store     *signin.KubeStore
	clients   *connect.Clients
	secrets   kube.Secrets
}

// providerSignIns builds the person's provider sign-ins: the sealing keys,
// the sign-in store, the connector and its browser pages, which sign the
// browser in to Dex at connect.SignInPath.
func providerSignIns(ctx context.Context, o *serveOptions, oauthCfg *server.OAuthConfig, clients *kube.Clients, providers []provider.Instance, log *slog.Logger) (*signIns, error) {
	if o.managerNamespace == "" || o.signInKeysSecret == "" || o.signInCurrentKey == "" {
		return nil, fmt.Errorf("provider sign-ins need --manager-namespace, --signin-keys-secret and --signin-current-key")
	}
	keyring, err := signin.LoadKeyring(ctx, clients.Typed(), o.managerNamespace, o.signInKeysSecret, o.signInCurrentKey)
	if err != nil {
		return nil, err
	}
	secrets := kube.Secrets{Client: clients.Typed(), Namespace: o.managerNamespace}
	oauthClients, err := connect.NewClients(providers, secrets, oauthCfg.BaseURL)
	if err != nil {
		return nil, err
	}
	store, err := signin.NewKubeStore(signin.Options{Client: clients.Typed(), Namespace: o.managerNamespace, Keyring: keyring, OAuth2: oauthClients, Logger: log})
	if err != nil {
		return nil, err
	}
	conn, err := connect.New(connect.Options{Clients: oauthClients, Store: store, Keyring: keyring, Logger: log})
	if err != nil {
		return nil, err
	}
	idp, err := oauthCfg.DexProvider(connect.SignInPath, log)
	if err != nil {
		return nil, err
	}
	pages, err := connect.NewPages(conn, idp)
	if err != nil {
		return nil, err
	}
	if o.grantSigningKey != "" {
		if err := checkGrantSigningKey(ctx, secrets, o.grantSigningKey); err != nil {
			return nil, err
		}
		log.Info("grant signing key read", "key", o.grantSigningKey)
	}
	log.Info("provider sign-ins enabled", "namespace", o.managerNamespace, "keysSecret", o.signInKeysSecret, "currentKey", keyring.Current())
	return &signIns{connector: conn, pages: pages, store: store, clients: oauthClients, secrets: secrets}, nil
}

// controllerConfig is the controller's configuration from the flags, the
// clients aside: a cycle, cleanup window or sizing that is out of range fails
// the start.
func (o *serveOptions) controllerConfig() (controller.Config, error) {
	cfg := controller.Config{Namespace: o.namespace, StorageClass: o.storageClass, Resync: o.syncCycle, SessionCleanupAfter: o.sessionCleanupAfter}
	if o.syncCycle <= 0 {
		return cfg, fmt.Errorf("--sync-cycle must be positive, got %s", o.syncCycle)
	}
	if o.sessionCleanupAfter <= 0 {
		return cfg, fmt.Errorf("--sessions-cleanup-after must be positive, got %s", o.sessionCleanupAfter)
	}
	headroom, err := resource.ParseQuantity(o.sizingHeadroom)
	if err != nil {
		return cfg, fmt.Errorf("--sizing-headroom %q: %w", o.sizingHeadroom, err)
	}
	cfg.Sizing = controller.Sizing{Factor: o.sizingFactor, Headroom: headroom}
	if o.sizingMaxSize != "" {
		maxSize, err := resource.ParseQuantity(o.sizingMaxSize)
		if err != nil {
			return cfg, fmt.Errorf("--sizing-max-size %q: %w", o.sizingMaxSize, err)
		}
		cfg.Sizing.MaxSize = &maxSize
	}
	if err := cfg.Sizing.Validate(); err != nil {
		return cfg, fmt.Errorf("sizing flags: %w", err)
	}
	return cfg, nil
}

// minGrantSigningKey is the shortest grant signing key accepted, in bytes.
const minGrantSigningKey = 32

// checkGrantSigningKey reads the grant signing key `<secret>/<key>` and
// refuses a malformed reference, a missing entry and a key shorter than
// minGrantSigningKey, so a misconfigured key fails the start instead of
// the first grant.
func checkGrantSigningKey(ctx context.Context, secrets kube.Secrets, ref string) error {
	name, key, ok := strings.Cut(ref, "/")
	if !ok || name == "" || key == "" {
		return fmt.Errorf("--grant-signing-key must be <secret>/<key>, got %q", ref)
	}
	v, err := secrets.Value(ctx, provider.SecretRef{Name: name, Key: key})
	if err != nil {
		return fmt.Errorf("grant signing key: %w", err)
	}
	if len(v) < minGrantSigningKey {
		return fmt.Errorf("grant signing key %s holds %d bytes, at least %d are needed", ref, len(v), minGrantSigningKey)
	}
	return nil
}

// tokenExchange configures POST /token for the installation's kagent client;
// nil without --token-exchange-client-id.
func tokenExchange(o *serveOptions, si *signIns, providers []provider.Instance, log *slog.Logger) (*exchange.Config, error) {
	if o.tokenExchangeClientID == "" {
		return nil, nil
	}
	name, key, ok := strings.Cut(o.tokenExchangeClientSecret, "/")
	if !ok || name == "" || key == "" {
		return nil, fmt.Errorf("--token-exchange-client-secret must be <secret>/<key>, got %q", o.tokenExchangeClientSecret)
	}
	log.Info("token exchange enabled", "client", o.tokenExchangeClientID, "clientSecret", name+"/"+key)
	return &exchange.Config{
		ClientID:     o.tokenExchangeClientID,
		ClientSecret: provider.SecretRef{Name: name, Key: key},
		Secrets:      si.secrets,
		Instances:    providers,
		Tokens:       si.store,
		ConnectURL:   si.clients.ConnectURL,
		Logger:       log,
	}, nil
}

// loadProviders reads and validates the provider instances.
func loadProviders(path string) ([]provider.Instance, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's own flag.
	if err != nil {
		return nil, fmt.Errorf("provider configuration: %w", err)
	}
	instances, err := kinds.Registry(nil).Load(data)
	if err != nil {
		return nil, fmt.Errorf("provider configuration %s:\n%w", path, err)
	}
	return instances, nil
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

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
