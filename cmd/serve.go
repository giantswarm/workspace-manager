package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/giantswarm/workspace-manager/internal/api"
	"github.com/giantswarm/workspace-manager/internal/authorize"
	"github.com/giantswarm/workspace-manager/internal/controller"
	"github.com/giantswarm/workspace-manager/internal/grant"
	"github.com/giantswarm/workspace-manager/internal/kagent"
	"github.com/giantswarm/workspace-manager/internal/kube"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/kinds"
	"github.com/giantswarm/workspace-manager/internal/server"
	"github.com/giantswarm/workspace-manager/internal/workspace"
)

// kubeFlags select how a command reaches the API server.
type kubeFlags struct {
	kubeconfig string
	context    string
	inCluster  bool
	qps        float64
	burst      int
}

func (k *kubeFlags) add(f *pflag.FlagSet) {
	f.StringVar(&k.kubeconfig, "kubeconfig", envOr("KUBECONFIG", ""), "Kubeconfig path; empty uses the default loading rules or in-cluster auth (KUBECONFIG)")
	f.StringVar(&k.context, "kube-context", envOr("KUBE_CONTEXT", ""), "Kubeconfig context override (KUBE_CONTEXT)")
	f.BoolVar(&k.inCluster, "in-cluster", envBool("KUBERNETES_IN_CLUSTER", false), "Force in-cluster Kubernetes auth (KUBERNETES_IN_CLUSTER)")
	f.Float64Var(&k.qps, "kube-qps", envFloat("KUBE_QPS", kube.DefaultQPS), "Requests per second to the API server, the whole process together; a tool call waits behind the controller's requests once they are spent (KUBE_QPS)")
	f.IntVar(&k.burst, "kube-burst", envInt("KUBE_BURST", kube.DefaultBurst), "Requests to the API server allowed at once above --kube-qps (KUBE_BURST)")
}

func (k *kubeFlags) config() kube.Config {
	return kube.Config{Kubeconfig: k.kubeconfig, Context: k.context, InCluster: k.inCluster, QPS: float32(k.qps), Burst: k.burst}
}

type serveOptions struct {
	listen    string
	readyWait time.Duration
	drain     time.Duration

	kube kubeFlags

	namespace           string
	organizations       []string
	organizationEmails  []string
	organizationDomains []string
	storageClass        string

	syncCycle           time.Duration
	sessionCleanupAfter time.Duration
	sessionPoll         time.Duration
	kagentAddress       string
	kagentTokenFile     string
	sizingFactor        float64
	sizingHeadroom      string
	sizingHeadroomShare float64
	sizingMaxSize       string

	syncJobTemplate     string
	syncImage           string
	syncImagePullPolicy string
	syncCAFile          string
	leaderElection      bool
	leaderIdentity      string

	mcpPath string

	providersConfig string

	managerNamespace    string
	musterProvider      string
	musterServer        string
	grantSigningKey     string
	grantTTL            time.Duration
	grantAudience       string
	grantBroker         string
	grantAgentNamespace string
	checkTimeout        time.Duration
	refreshAfter        time.Duration

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
	f.DurationVar(&o.readyWait, "ready-provider-wait", envDuration("WORKSPACE_MANAGER_READY_PROVIDER_WAIT", provider.DefaultReachWait), "How long a starting replica stays unready until every provider instance answers once, a Go duration; past it the replica is Ready anyway (WORKSPACE_MANAGER_READY_PROVIDER_WAIT)")
	f.DurationVar(&o.drain, "shutdown-drain", envDuration("WORKSPACE_MANAGER_SHUTDOWN_DRAIN", 10*time.Second), "How long a stopping replica keeps serving, unready, before it closes its listener: the time its Service takes to stop sending it connections, a Go duration (WORKSPACE_MANAGER_SHUTDOWN_DRAIN)")
	o.kube.add(f)
	f.StringVar(&o.namespace, "namespace", envOr("WORKSPACE_MANAGER_NAMESPACE", "kagent"), "Namespace the workspaces and everything they own live in (WORKSPACE_MANAGER_NAMESPACE)")
	f.StringArrayVar(&o.organizations, "organization", nil, "An Organization and its member groups, `<organization>=<group>[,<group>...]`, once per Organization: a caller carrying any of the groups reads and writes the Organization's workspaces; an Organization not given has no members")
	f.StringArrayVar(&o.organizationEmails, "organization-emails", nil, "An Organization and its member emails, `<organization>=<email>[,<email>...]`, once per Organization: a caller whose verified email is one of them is a member, for an identity provider that sends no groups")
	f.StringArrayVar(&o.organizationDomains, "organization-domains", nil, "An Organization and its member email domains, `<organization>=<domain>[,<domain>...]`, once per Organization: a caller whose verified email ends in `@<domain>` is a member, no subdomain, for an identity provider that sends no groups")
	f.StringVar(&o.storageClass, "storage-class", envOr("WORKSPACE_MANAGER_STORAGE_CLASS", ""), "The read-write-many StorageClass every workspace's volume is claimed from; empty claims none: each Workspace's VolumeClaimed condition names the missing value, and the cluster's default class is never used in its place (WORKSPACE_MANAGER_STORAGE_CLASS)")
	f.DurationVar(&o.syncCycle, "sync-cycle", envDuration("WORKSPACE_MANAGER_SYNC_CYCLE", controller.DefaultResync), "How often the controller reconciles every Workspace without an event, and how long after a failed sync cycle it tries again, a Go duration (WORKSPACE_MANAGER_SYNC_CYCLE)")
	f.DurationVar(&o.sessionCleanupAfter, "sessions-cleanup-after", envDuration("WORKSPACE_MANAGER_SESSIONS_CLEANUP_AFTER", controller.DefaultSessionCleanupAfter), "How long a Session's directory outlives the Session's last turn, a Go duration; a directory whose Session kagent does not know outlives the sync that first found it as long (WORKSPACE_MANAGER_SESSIONS_CLEANUP_AFTER)")
	f.DurationVar(&o.sessionPoll, "sessions-poll", envDuration("WORKSPACE_MANAGER_SESSIONS_POLL", controller.DefaultSessionPoll), "How often kagent's Sessions are listed for one deleted since the last sync, whose directory then goes at once in a cleanup-only sync Job, a Go duration; one listing serves every workspace (WORKSPACE_MANAGER_SESSIONS_POLL)")
	f.StringVar(&o.kagentAddress, "kagent-address", envOr("WORKSPACE_MANAGER_KAGENT_ADDRESS", ""), "The kagent controller's gRPC API, grpc://host:port or grpcs://host[:port], whose Sessions' last turns the session directories' cleanup works from; empty cleans up no session directory, and a deleted workspace keeps its volume (WORKSPACE_MANAGER_KAGENT_ADDRESS)")
	f.StringVar(&o.kagentTokenFile, "kagent-token-file", envOr("WORKSPACE_MANAGER_KAGENT_TOKEN_FILE", ""), "File with the token the manager calls kagent with, read per call: a projected ServiceAccount token for kagent's audience; empty sends none (WORKSPACE_MANAGER_KAGENT_TOKEN_FILE)")
	f.Float64Var(&o.sizingFactor, "sizing-factor", envFloat("WORKSPACE_MANAGER_SIZING_FACTOR", controller.DefaultSizing.Factor), "What a repository not yet mirrored counts towards its workspace volume's need: this factor, at least 1, times the size its provider reports; a mirrored one counts its measured size (WORKSPACE_MANAGER_SIZING_FACTOR)")
	f.StringVar(&o.sizingHeadroom, "sizing-headroom", envOr("WORKSPACE_MANAGER_SIZING_HEADROOM", controller.DefaultSizing.HeadroomFloor.String()), "The least headroom a workspace volume keeps over its need, a Kubernetes quantity; the headroom is the larger of this, the headroom share of the need and the Workspace's own spec.sizing.headroom (WORKSPACE_MANAGER_SIZING_HEADROOM)")
	f.Float64Var(&o.sizingHeadroomShare, "sizing-headroom-share", envFloat("WORKSPACE_MANAGER_SIZING_HEADROOM_SHARE", controller.DefaultSizing.HeadroomShare), "The headroom as a share of a workspace volume's need, 0.5 for half of it; the larger of it and --sizing-headroom applies (WORKSPACE_MANAGER_SIZING_HEADROOM_SHARE)")
	f.StringVar(&o.sizingMaxSize, "sizing-max-size", envOr("WORKSPACE_MANAGER_SIZING_MAX_SIZE", ""), "Ceiling of a workspace volume's size, a Kubernetes quantity: a sync cycle that would pass it fails, naming the size it needs, and keeps the mirrors it has; empty sets none (WORKSPACE_MANAGER_SIZING_MAX_SIZE)")
	f.StringVar(&o.syncJobTemplate, "sync-job-template", envOr("WORKSPACE_MANAGER_SYNC_JOB_TEMPLATE", ""), "A Job manifest (YAML) every sync Job is made from, the chart's rendering of its image, account, security contexts, resources, deadline and TTL; it needs a container named `sync` running `workspace-manager sync`, and the cycle adds the volume, the selection and the credentials. Required: serve refuses to start without it (WORKSPACE_MANAGER_SYNC_JOB_TEMPLATE)")
	f.StringVar(&o.syncImage, "sync-image", envOr("WORKSPACE_MANAGER_SYNC_IMAGE", ""), "The image the sync Job runs, this binary's own; empty keeps the template's, and serve refuses to start when neither names one (WORKSPACE_MANAGER_SYNC_IMAGE)")
	f.StringVar(&o.syncImagePullPolicy, "sync-image-pull-policy", envOr("WORKSPACE_MANAGER_SYNC_IMAGE_PULL_POLICY", ""), "The sync image's pull policy, with --sync-image (WORKSPACE_MANAGER_SYNC_IMAGE_PULL_POLICY)")
	f.StringVar(&o.syncCAFile, "sync-ca-file", envOr("WORKSPACE_MANAGER_SYNC_CA_FILE", os.Getenv("SSL_CERT_FILE")), "PEM CA bundle the sync Job verifies the providers' git hosts with (a GitHub Enterprise Server behind a private CA), read at start and handed to every sync Job in its input: this process's own SSL_CERT_FILE unless set; empty hands none and the image's roots apply (WORKSPACE_MANAGER_SYNC_CA_FILE)")
	f.BoolVar(&o.leaderElection, "leader-election", envBool("WORKSPACE_MANAGER_LEADER_ELECTION", true), "Elect one replica to run the controller, through the Lease `"+controller.DefaultLease+"` in the workspaces' namespace; off runs it in every replica, for a single local process only (WORKSPACE_MANAGER_LEADER_ELECTION)")
	f.StringVar(&o.leaderIdentity, "leader-election-identity", envOr("POD_NAME", ""), "This replica's name in the election; empty takes the host name, the pod's (POD_NAME)")
	f.StringVar(&o.providersConfig, "providers-config", envOr("WORKSPACE_MANAGER_PROVIDERS_CONFIG", ""), "Provider instances file (YAML, the chart's `providers`); empty configures none (WORKSPACE_MANAGER_PROVIDERS_CONFIG)")
	f.StringVar(&o.managerNamespace, "manager-namespace", envOr("POD_NAMESPACE", ""), "The manager's own namespace, where the provider Secrets and the grant's signing key live; the provider sign-ins earlier versions kept there are deleted at start (POD_NAMESPACE)")
	f.StringVar(&o.musterProvider, "muster-provider", envOr("WORKSPACE_MANAGER_MUSTER_PROVIDER", ""), "The provider instance whose App muster pins this server's MCPServer to (auth.authorizationServer, grantScope subject, forwardIdentity): the bearer of every MCP call is the person's App token, muster's grant, verified with the provider, and the person's Dex ID token arrives in "+server.ForwardedIdentityHeader+". It is the person's token for that provider in authorize_workspace and list_providers; muster holds no grant of any other instance. Empty: no person's provider token, and authorize_workspace asks to grant every provider. Needs --enable-oauth (WORKSPACE_MANAGER_MUSTER_PROVIDER)")
	f.StringVar(&o.musterServer, "muster-server", envOr("WORKSPACE_MANAGER_MUSTER_SERVER", "workspace-manager"), "This server's MCPServer name in muster, which the refusals name for core_auth_login (WORKSPACE_MANAGER_MUSTER_SERVER)")
	f.StringVar(&o.grantSigningKey, "grant-signing-key", envOr("WORKSPACE_MANAGER_GRANT_SIGNING_KEY", ""), "The key the workspace grant a Session carries is signed with, as `<secret>/<key>` of a Secret in the manager's namespace: a PEM private key, EC P-256 or P-384 or RSA of at least 2048 bits. Every other entry of the Secret holding such a key is published beside it at "+grant.JWKSPath+", so a key rotates by adding the next one, switching to it and removing the old one. Empty serves no authorize_workspace. Needs providers, OAuth and --manager-namespace (WORKSPACE_MANAGER_GRANT_SIGNING_KEY)")
	f.DurationVar(&o.grantTTL, "grant-ttl", envDuration("WORKSPACE_MANAGER_GRANT_TTL", grant.DefaultTTL), "How long a workspace grant is valid, a Go duration (WORKSPACE_MANAGER_GRANT_TTL)")
	f.StringVar(&o.grantAudience, "grant-audience", envOr("WORKSPACE_MANAGER_GRANT_AUDIENCE", "kagent"), "The workspace grant's audience: the one kagent's Session admission requires (WORKSPACE_MANAGER_GRANT_AUDIENCE)")
	f.StringVar(&o.grantBroker, "grant-broker", envOr("WORKSPACE_MANAGER_GRANT_BROKER", "workspace-manager"), "The token broker the egress gateway fetches the person's token from, as kagent's Session admission knows it, which every provider host of a grant names (WORKSPACE_MANAGER_GRANT_BROKER)")
	f.StringVar(&o.grantAgentNamespace, "grant-agent-namespace", envOr("WORKSPACE_MANAGER_GRANT_AGENT_NAMESPACE", ""), "The namespace whose Agents may start a Session with a workspace grant, its `agent_namespace`: kagent's Session admission refuses one on an Agent elsewhere. Required with --grant-signing-key (WORKSPACE_MANAGER_GRANT_AGENT_NAMESPACE)")
	f.DurationVar(&o.checkTimeout, "access-check-timeout", envDuration("WORKSPACE_MANAGER_ACCESS_CHECK_TIMEOUT", authorize.DefaultCheckTimeout), "How long authorize_workspace waits for the providers' answers about the person's access before it refuses, saying so, a Go duration; below the MCP client's tool timeout (muster's MCPServer `timeout`, 30s by default) (WORKSPACE_MANAGER_ACCESS_CHECK_TIMEOUT)")
	f.DurationVar(&o.refreshAfter, "sync-refresh-after", envDuration("WORKSPACE_MANAGER_SYNC_REFRESH_AFTER", authorize.DefaultRefreshAfter), "The refresh threshold: authorize_workspace requests a sync of a workspace whose last sync is older, without the Session waiting for it, a Go duration (WORKSPACE_MANAGER_SYNC_REFRESH_AFTER)")
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
	orgs, err := workspace.ParseOrganizations(o.organizations, o.organizationEmails, o.organizationDomains)
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
	// A CRD that prunes the status the controller records a running sync
	// Job in would have it start sync Jobs without end: refuse it at once.
	if err := controller.CheckCRD(ctx, clients.Dynamic()); err != nil {
		return err
	}
	ctlCfg.Dynamic, ctlCfg.Core, ctlCfg.Logger = clients.Dynamic(), clients.Typed(), log
	// The controller claims each Workspace's volume; without a class it
	// claims none and says so in every Workspace's condition.
	if o.storageClass == "" {
		log.Warn("no StorageClass: no workspace volume is claimed until --storage-class (the chart's storage.storageClassName) names the installation's read-write-many class")
	}
	// The sync cycle lists each workspace's owners and fetches with the
	// provider instances' sync credentials, read from their Secrets.
	ctlCfg.Instances = providers
	if o.managerNamespace != "" {
		// The person's token is muster's grant: no sign-in an earlier
		// version stored stays in the cluster.
		if err := kube.PurgeSignIns(ctx, clients.Typed(), o.managerNamespace); err != nil {
			return err
		}
		secrets := kube.Secrets{Client: clients.Typed(), Namespace: o.managerNamespace}
		ctlCfg.Secrets = secrets
		ctlCfg.Listings = workspace.NewListings(providers, secrets)
	} else if len(providers) > 0 {
		log.Warn("the sync cycle is off: it reads the providers' Secrets in --manager-namespace (POD_NAMESPACE); every cycle fails until it is set")
	}
	// The session directories' cleanup reads every Session's last turn from
	// kagent; without it, no directory is cleaned up.
	if o.kagentAddress != "" {
		sessions, err := kagent.Dial(o.kagentAddress, o.kagentTokenFile)
		if err != nil {
			return err
		}
		defer func() { _ = sessions.Close() }()
		ctlCfg.Sessions = sessions
	} else {
		log.Warn("no kagent: no session directory is cleaned up, and a deleted workspace keeps its volume, until --kagent-address (the chart's kagent.address) names kagent's API", "sessionsCleanupAfter", o.sessionCleanupAfter)
	}
	ctl := controller.New(ctlCfg)

	if o.readyWait <= 0 {
		return fmt.Errorf("--ready-provider-wait must be positive, got %s", o.readyWait)
	}
	if o.drain < 0 {
		return fmt.Errorf("--shutdown-drain must not be negative, got %s", o.drain)
	}
	// Readiness waits for the providers' first answer: a replica the Service
	// sends a Session start to reaches them.
	reach := &provider.Reach{Instances: providers, Wait: o.readyWait, Logger: log}
	srvCfg := server.Config{Addr: o.listen, MCPPath: o.mcpPath, Ready: reach.Ready, Drain: o.drain}
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
	apiCfg := api.Config{Kube: clients, Namespace: o.namespace, Providers: providers, Organizations: orgs, Sizing: ctlCfg.Sizing}
	if o.managerNamespace != "" {
		// The provider instances' Secrets: their sync credentials list an
		// owner's repositories for list_repositories and preview_workspace.
		apiCfg.Secrets = kube.Secrets{Client: clients.Typed(), Namespace: o.managerNamespace}
	} else if len(providers) > 0 {
		log.Warn("list_repositories and preview_workspace are off: they read the providers' Secrets in --manager-namespace (POD_NAMESPACE)")
	}
	grants, pin, err := musterGrants(o, providers)
	if err != nil {
		return err
	}
	apiCfg.Grants, srvCfg.Pin = grants, pin
	if pin != nil && srvCfg.OAuth == nil {
		return fmt.Errorf("--muster-provider needs --enable-oauth, which validates the forwarded Dex ID token")
	}
	if o.grantSigningKey != "" {
		if len(providers) == 0 || srvCfg.OAuth == nil || o.managerNamespace == "" {
			return fmt.Errorf("--grant-signing-key needs providers, --enable-oauth and --manager-namespace")
		}
		if apiCfg.Authorizer, srvCfg.JWKS, err = workspaceGrants(ctx, o, grants, clients, orgs, providers, log); err != nil {
			return err
		}
	}
	srv, err := server.New(srvCfg, api.NewMCPServer(apiCfg, build.Version), log)
	if err != nil {
		return err
	}
	log.Info("workspace-manager starting", "version", build.Version, "commit", build.Commit, "listen", o.listen,
		"mcp", o.mcpPath, "oauth", o.oauthEnabled, "namespace", o.namespace, "organizations", len(orgs), "storageClass", o.storageClass,
		"syncJobTemplate", o.syncJobTemplate, "leaderElection", o.leaderElection)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The server and the controller stop together: on a signal, or when one
	// of them fails.
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return srv.Run(ctx) })
	g.Go(func() error { return ctl.Run(ctx) })
	g.Go(func() error { reach.Run(ctx); return nil })
	return g.Wait()
}

// musterGrants are the person's provider tokens as muster attaches them, and
// the pin of this server's registration to --muster-provider's App; no pin
// without it. A pinned instance the installation does not configure fails the
// start.
func musterGrants(o *serveOptions, providers []provider.Instance) (muster.Grants, *server.Pin, error) {
	grants := muster.Grants{Instance: o.musterProvider, Server: o.musterServer}
	if o.musterProvider == "" {
		return grants, nil, nil
	}
	if o.musterServer == "" {
		return grants, nil, errors.New("--muster-server must name this server's MCPServer in muster")
	}
	for _, p := range providers {
		if p.Name == o.musterProvider {
			return grants, &server.Pin{Grants: grants, Kind: p.Kind}, nil
		}
	}
	return grants, nil, fmt.Errorf("--muster-provider %q names no configured provider instance", o.musterProvider)
}

// controllerConfig is the controller's configuration from the flags, the
// clients aside: a cycle, cleanup window or sizing that is out of range, or a
// sync Job without its template or an image, fails the start.
func (o *serveOptions) controllerConfig() (controller.Config, error) {
	cfg := controller.Config{
		Namespace: o.namespace, StorageClass: o.storageClass, Resync: o.syncCycle, SessionCleanupAfter: o.sessionCleanupAfter,
		SessionPoll: o.sessionPoll,
		SyncImage:   o.syncImage, SyncImagePullPolicy: corev1.PullPolicy(o.syncImagePullPolicy),
	}
	if o.syncCycle <= 0 {
		return cfg, fmt.Errorf("--sync-cycle must be positive, got %s", o.syncCycle)
	}
	if o.sessionCleanupAfter <= 0 {
		return cfg, fmt.Errorf("--sessions-cleanup-after must be positive, got %s", o.sessionCleanupAfter)
	}
	if o.sessionPoll <= 0 {
		return cfg, fmt.Errorf("--sessions-poll must be positive, got %s", o.sessionPoll)
	}
	headroom, err := resource.ParseQuantity(o.sizingHeadroom)
	if err != nil {
		return cfg, fmt.Errorf("--sizing-headroom %q: %w", o.sizingHeadroom, err)
	}
	cfg.Sizing = controller.Sizing{Factor: o.sizingFactor, HeadroomFloor: headroom, HeadroomShare: o.sizingHeadroomShare}
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
	switch cfg.SyncImagePullPolicy {
	case "", corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
	default:
		return cfg, fmt.Errorf("--sync-image-pull-policy must be Always, IfNotPresent or Never, got %q", o.syncImagePullPolicy)
	}
	// A sync Job without a template or an image fails the start, not every
	// cycle: the API server refuses a Job with an empty image.
	if o.syncJobTemplate == "" {
		return cfg, errors.New("--sync-job-template is required: the sync Job is made from the chart's template, with --sync-image or the template's image")
	}
	if cfg.SyncJobTemplate, err = controller.LoadJobTemplate(o.syncJobTemplate); err != nil {
		return cfg, err
	}
	if err := (controller.Jobs{Template: cfg.SyncJobTemplate, Image: cfg.SyncImage}).Validate(); err != nil {
		return cfg, fmt.Errorf("sync job template %s: %w; set --sync-image or the template's image", o.syncJobTemplate, err)
	}
	if o.syncCAFile != "" {
		// Unreadable roots fail the start, not every fetch.
		if cfg.SyncRoots, err = os.ReadFile(o.syncCAFile); err != nil { //nolint:gosec // the CA path is the operator's own flag
			return cfg, fmt.Errorf("--sync-ca-file: %w", err)
		}
		if len(bytes.TrimSpace(cfg.SyncRoots)) == 0 {
			return cfg, fmt.Errorf("--sync-ca-file %s is empty", o.syncCAFile)
		}
	}
	if o.leaderElection {
		identity := o.leaderIdentity
		if identity == "" {
			if identity, err = os.Hostname(); err != nil {
				return cfg, fmt.Errorf("leader election identity: %w", err)
			}
		}
		cfg.LeaderElection = &controller.LeaderElection{Identity: identity}
	}
	return cfg, nil
}

// workspaceGrants builds authorize_workspace and the grant's JWKS, the grant
// issued by this server's public URL. The signing key is read now, so a missing or
// unusable key fails the start instead of the first grant.
func workspaceGrants(ctx context.Context, o *serveOptions, grants muster.Grants, clients kube.Client, orgs workspace.Organizations,
	providers []provider.Instance, log *slog.Logger,
) (*authorize.Authorizer, http.Handler, error) {
	name, key, ok := strings.Cut(o.grantSigningKey, "/")
	if !ok || name == "" || key == "" {
		return nil, nil, fmt.Errorf("--grant-signing-key must be <secret>/<key>, got %q", o.grantSigningKey)
	}
	if o.grantTTL <= 0 {
		return nil, nil, fmt.Errorf("--grant-ttl must be positive, got %s", o.grantTTL)
	}
	keys := &grant.KeySource{Client: clients.Typed(), Namespace: o.managerNamespace, Secret: name, Key: key}
	k, err := keys.Keys(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, len(providers))
	for i, p := range providers {
		names[i] = p.Name
	}
	a, err := authorize.New(authorize.Config{
		Workspaces:     workspace.NewStore(clients.Dynamic(), o.namespace, orgs, names),
		Core:           clients.Typed(),
		Namespace:      o.namespace,
		Instances:      providers,
		Grants:         grants,
		Signer:         &grant.Signer{Keys: keys, Issuer: o.oauthBaseURL, Audience: o.grantAudience, TTL: o.grantTTL},
		Broker:         o.grantBroker,
		AgentNamespace: o.grantAgentNamespace,
		CheckTimeout:   o.checkTimeout,
		RefreshAfter:   o.refreshAfter,
	})
	if err != nil {
		return nil, nil, err
	}
	log.Info("workspace grants enabled", "signingKey", o.grantSigningKey, "keyID", k.Signing.KeyID, "published", len(k.Public.Keys),
		"issuer", o.oauthBaseURL, "audience", o.grantAudience, "ttl", o.grantTTL, "broker", o.grantBroker, "agentNamespace", o.grantAgentNamespace)
	return a, grant.JWKS(keys, log), nil
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
