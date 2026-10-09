// Package kube builds the Kubernetes clients workspace-manager works with:
// its own ServiceAccount's. The manager checks the caller's Organization from
// the forwarded identity and then writes the workspace objects, the volume and
// the jobs itself, so its RBAC, not the caller's, bounds what it can touch.
package kube

import (
	"fmt"
	"os"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
}

// New builds the clients.
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
