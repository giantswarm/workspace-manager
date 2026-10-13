// Package kube builds the Kubernetes clients workspace-manager works with:
// its own ServiceAccount's. The manager checks the caller's Organization from
// the forwarded identity and then writes the workspace objects, the volume and
// the jobs itself, so its RBAC, not the caller's, bounds what it can touch.
package kube

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
)

// Client is what the tools need from Kubernetes: the dynamic client for the
// custom resources, the typed client for the core ones and discovery for the
// served API versions.
type Client interface {
	Dynamic() dynamic.Interface
	Typed() kubernetes.Interface
	Discovery() discovery.DiscoveryInterface
}

// Clients is the concrete client set built from a rest.Config.
type Clients struct {
	dynamic   dynamic.Interface
	typed     kubernetes.Interface
	discovery discovery.DiscoveryInterface
}

// Dynamic implements Client.
func (c *Clients) Dynamic() dynamic.Interface { return c.dynamic }

// Typed implements Client.
func (c *Clients) Typed() kubernetes.Interface { return c.typed }

// Discovery implements Client.
func (c *Clients) Discovery() discovery.DiscoveryInterface { return c.discovery }

// Config selects how to reach the API server.
type Config struct {
	// Kubeconfig is an explicit kubeconfig path; empty uses the default
	// loading rules ($KUBECONFIG, ~/.kube/config).
	Kubeconfig string
	// Context overrides the kubeconfig's current context.
	Context string
	// InCluster forces in-cluster auth. When false and no kubeconfig is
	// found, in-cluster auth is still tried if the pod environment is present.
	InCluster bool
	// QPS and Burst bound the requests to the API server, every client of the
	// process together: the controller, the sign-ins and the tools a person
	// waits on draw from one budget. Zero is DefaultQPS and DefaultBurst.
	QPS   float32
	Burst int
	// Logger records a request that waited for the budget; nil is
	// slog.Default.
	Logger *slog.Logger
}

// DefaultQPS and DefaultBurst size the budget for a server that answers
// people while its controller syncs: client-go's own 5 and 10 hold a tool
// call behind a running sync cycle's requests for a second. The API server's
// priority and fairness protects it beyond that.
const (
	DefaultQPS   = 50
	DefaultBurst = 100
)

// New builds the clients.
func New(cfg Config) (*Clients, error) {
	if cfg.QPS < 0 || cfg.Burst < 0 {
		return nil, fmt.Errorf("kubernetes client budget: QPS and burst must not be negative, got %g and %d", cfg.QPS, cfg.Burst)
	}
	restCfg, err := restConfig(cfg)
	if err != nil {
		return nil, err
	}
	restCfg.UserAgent = "workspace-manager"
	restCfg.RateLimiter = newRateLimiter(cfg)
	return fromRESTConfig(restCfg)
}

// throttledAfter is the wait for the budget worth a log line: client-go's own
// threshold for its throttling message.
const throttledAfter = 50 * time.Millisecond

// loggedLimiter is the process's token bucket, logging every request that
// waited for it.
type loggedLimiter struct {
	flowcontrol.RateLimiter
	log *slog.Logger
}

func newRateLimiter(cfg Config) flowcontrol.RateLimiter {
	qps, burst := cfg.QPS, cfg.Burst
	if qps == 0 {
		qps = DefaultQPS
	}
	if burst == 0 {
		burst = DefaultBurst
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &loggedLimiter{RateLimiter: flowcontrol.NewTokenBucketRateLimiter(qps, burst), log: log}
}

// Wait implements flowcontrol.RateLimiter, the call every request makes.
func (l *loggedLimiter) Wait(ctx context.Context) error {
	start := time.Now()
	err := l.RateLimiter.Wait(ctx)
	if waited := time.Since(start); waited >= throttledAfter {
		l.log.DebugContext(ctx, "kubernetes request waited for the client's budget", "waited", waited, "qps", l.QPS())
	}
	return err
}

func fromRESTConfig(restCfg *rest.Config) (*Clients, error) {
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("discovery client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	return &Clients{dynamic: dyn, typed: cs, discovery: disc}, nil
}

func restConfig(cfg Config) (*rest.Config, error) {
	if cfg.InCluster {
		c, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster config: %w", err)
		}
		return c, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Kubeconfig != "" {
		rules.ExplicitPath = cfg.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{}
	if cfg.Context != "" {
		overrides.CurrentContext = cfg.Context
	}
	c, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err == nil {
		return c, nil
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		if ic, icErr := rest.InClusterConfig(); icErr == nil {
			return ic, nil
		}
	}
	return nil, fmt.Errorf("kubeconfig: %w", err)
}
