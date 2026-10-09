// Package kube builds the Kubernetes clients workspace-manager works with.
// Every call a request makes presents the caller's own Dex token, so the
// apiserver authenticates the person and the person's RBAC decides; the pod's
// ServiceAccount contributes only the in-cluster API address and CA.
package kube

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

// ErrNoCallerToken is returned when the request carries no Dex token: the
// server acts as the caller only, and there is no other credential to fall
// back to.
var ErrNoCallerToken = errors.New("no caller token on the request")

// Client is what the tools need from Kubernetes: the dynamic client for the
// custom resources, the typed client for the core ones and discovery for the
// served API versions.
type Client interface {
	Dynamic() dynamic.Interface
	Typed() kubernetes.Interface
	Discovery() discovery.DiscoveryInterface
}

// Provider resolves the client a request runs with.
type Provider interface {
	Client(ctx context.Context) (Client, error)
}

// Clients is the concrete client set built from a rest.Config.
type Clients struct {
	dynamic   dynamic.Interface
	typed     kubernetes.Interface
	discovery discovery.DiscoveryInterface
	restCfg   *rest.Config
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
}

// New builds the base clients.
func New(cfg Config) (*Clients, error) {
	restCfg, err := restConfig(cfg)
	if err != nil {
		return nil, err
	}
	restCfg.UserAgent = "workspace-manager"
	return fromRESTConfig(restCfg)
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
	return &Clients{dynamic: dyn, typed: cs, discovery: disc, restCfg: restCfg}, nil
}

// ForToken returns clients that authenticate to the API server with token (a
// Dex id_token the apiserver trusts). Server address, CA and TLS settings are
// kept; every credential of the base config is dropped so nothing but the
// caller's token is presented.
func (c *Clients) ForToken(token string) (*Clients, error) {
	if token == "" {
		return nil, ErrNoCallerToken
	}
	if c.restCfg == nil {
		return nil, fmt.Errorf("caller clients need a REST config (built with New)")
	}
	cfg := rest.AnonymousClientConfig(c.restCfg)
	cfg.BearerToken = token
	cfg.UserAgent = c.restCfg.UserAgent
	return fromRESTConfig(cfg)
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

// CallerProvider hands out clients that present the caller's Dex token (put on
// the context by the OAuth layer). A request without a token gets
// ErrNoCallerToken; a token that expires while its request is still running
// keeps being presented, so the apiserver answers 401 and the request fails
// attributed to the caller. Clients are cached per token until the token's
// exp.
type CallerProvider struct {
	base *Clients
	log  *slog.Logger

	mu      sync.Mutex
	byToken map[[32]byte]*callerEntry
}

type callerEntry struct {
	clients *Clients
	expires time.Time
	// expiredLogged: the expiry is logged once per token, not on every call
	// a request makes after its token ran out.
	expiredLogged bool
}

// maxCallerClients bounds the per-token cache; entries expire with their token
// anyway, this only guards against a flood of distinct tokens.
const maxCallerClients = 256

// NewCallerProvider builds per-caller clients from base (its server address,
// CA and TLS settings; never its credentials).
func NewCallerProvider(base *Clients, log *slog.Logger) *CallerProvider {
	if log == nil {
		log = slog.Default()
	}
	return &CallerProvider{base: base, log: log, byToken: map[[32]byte]*callerEntry{}}
}

// Client implements Provider.
func (p *CallerProvider) Client(ctx context.Context) (Client, error) {
	token, ok := identity.TokenFromContext(ctx)
	if !ok {
		return nil, ErrNoCallerToken
	}
	key := sha256.Sum256([]byte(token))
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	e, cached := p.byToken[key]
	if !cached {
		if len(p.byToken) >= maxCallerClients {
			p.evictLocked(now)
		}
		clients, err := p.base.ForToken(token)
		if err != nil {
			return nil, fmt.Errorf("caller clients: %w", err)
		}
		e = &callerEntry{clients: clients, expires: identity.TokenExpiry(token)}
		p.byToken[key] = e
	}
	if !e.expires.IsZero() && now.After(e.expires) && !e.expiredLogged {
		e.expiredLogged = true
		p.log.Warn("caller token expired; the Kubernetes API will reject the remaining calls of this request",
			identity.LogAttr(ctx), "expired", e.expires.UTC().Format(time.RFC3339))
	}
	return e.clients, nil
}

// evictLocked drops expired entries, and when nothing expired the whole cache
// (a token flood is not worth an LRU).
func (p *CallerProvider) evictLocked(now time.Time) {
	for k, e := range p.byToken {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(p.byToken, k)
		}
	}
	if len(p.byToken) >= maxCallerClients {
		p.byToken = map[[32]byte]*callerEntry{}
	}
}
