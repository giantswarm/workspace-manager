package cmd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/workspace-manager/internal/connect"
	"github.com/giantswarm/workspace-manager/internal/controller"
	"github.com/giantswarm/workspace-manager/internal/kube"
	"github.com/giantswarm/workspace-manager/internal/signin"
)

func TestLoadProvidersFailsTheStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
providers:
  - name: github
    kind: github
    values:
      app: {id: "1", privateKey: {name: app, key: private-key}}
      oauth: {clientID: c, clientSecret: {name: app, key: client-secret}}
  - name: github
    kind: gitlab
`), 0o600))
	_, err := loadProviders(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	assert.Contains(t, err.Error(), `provider "github": duplicate name`)
}

func TestLoadProvidersNone(t *testing.T) {
	instances, err := loadProviders("")
	require.NoError(t, err)
	assert.Empty(t, instances)
}

// TestServeStorageClassFlag: --storage-class is empty by default (the
// controller claims nothing), reads its environment variable, and the flag
// wins over it.
func TestServeStorageClassFlag(t *testing.T) {
	flag := newServeCmd().Flags().Lookup("storage-class")
	require.NotNil(t, flag)
	assert.Empty(t, flag.DefValue)
	assert.Contains(t, flag.Usage, "WORKSPACE_MANAGER_STORAGE_CLASS")

	t.Setenv("WORKSPACE_MANAGER_STORAGE_CLASS", "efs-rwx")
	cmd := newServeCmd()
	assert.Equal(t, "efs-rwx", cmd.Flags().Lookup("storage-class").DefValue)
	require.NoError(t, cmd.Flags().Parse([]string{"--storage-class=azurefile-nfs"}))
	got, err := cmd.Flags().GetString("storage-class")
	require.NoError(t, err)
	assert.Equal(t, "azurefile-nfs", got)
}

// TestServeControllerFlags: the cycle, the cleanup window and the sizing
// default to the controller's own values and each reads its environment
// variable.
func TestServeControllerFlags(t *testing.T) {
	f := newServeCmd().Flags()
	for name, def := range map[string]string{
		"sync-cycle":             controller.DefaultResync.String(),
		"sessions-cleanup-after": controller.DefaultSessionCleanupAfter.String(),
		"sizing-factor":          "1.5",
		"sizing-headroom":        "5Gi",
		"sizing-max-size":        "",
		"grant-signing-key":      "",
	} {
		flag := f.Lookup(name)
		require.NotNil(t, flag, name)
		assert.Equal(t, def, flag.DefValue, name)
	}

	t.Setenv("WORKSPACE_MANAGER_SYNC_CYCLE", "30m")
	t.Setenv("WORKSPACE_MANAGER_SESSIONS_CLEANUP_AFTER", "168h")
	t.Setenv("WORKSPACE_MANAGER_SIZING_FACTOR", "2")
	f = newServeCmd().Flags()
	assert.Equal(t, "30m0s", f.Lookup("sync-cycle").DefValue)
	assert.Equal(t, "168h0m0s", f.Lookup("sessions-cleanup-after").DefValue)
	assert.Equal(t, "2", f.Lookup("sizing-factor").DefValue)
}

func validServeOptions() *serveOptions {
	return &serveOptions{namespace: "kagent", storageClass: "efs-rwx", syncCycle: time.Hour, sessionCleanupAfter: 720 * time.Hour,
		sizingFactor: 2, sizingHeadroom: "10Gi", sizingMaxSize: "500Gi",
		grantTTL: 5 * time.Minute, grantAudience: "kagent", grantBroker: "workspace-manager"}
}

func TestServeControllerConfig(t *testing.T) {
	cfg, err := validServeOptions().controllerConfig()
	require.NoError(t, err)
	assert.Equal(t, time.Hour, cfg.Resync)
	assert.Equal(t, 720*time.Hour, cfg.SessionCleanupAfter)
	assert.Equal(t, "efs-rwx", cfg.StorageClass)
	assert.InDelta(t, 2.0, cfg.Sizing.Factor, 0)
	assert.Equal(t, "10Gi", cfg.Sizing.Headroom.String())
	require.NotNil(t, cfg.Sizing.MaxSize)
	assert.Equal(t, "500Gi", cfg.Sizing.MaxSize.String())

	o := validServeOptions()
	o.sizingMaxSize = ""
	cfg, err = o.controllerConfig()
	require.NoError(t, err)
	assert.Nil(t, cfg.Sizing.MaxSize, "empty sets no ceiling")
}

// TestServeControllerConfigFailsTheStart: a value out of range names its flag.
func TestServeControllerConfigFailsTheStart(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*serveOptions)
		want   string
	}{
		"zero cycle":        {func(o *serveOptions) { o.syncCycle = 0 }, "--sync-cycle"},
		"negative cleanup":  {func(o *serveOptions) { o.sessionCleanupAfter = -time.Hour }, "--sessions-cleanup-after"},
		"bad headroom":      {func(o *serveOptions) { o.sizingHeadroom = "lots" }, "--sizing-headroom"},
		"bad max size":      {func(o *serveOptions) { o.sizingMaxSize = "huge" }, "--sizing-max-size"},
		"factor below one":  {func(o *serveOptions) { o.sizingFactor = 0.5 }, "sizing factor"},
		"negative headroom": {func(o *serveOptions) { o.sizingHeadroom = "-1Gi" }, "headroom must not be negative"},
		"zero max size":     {func(o *serveOptions) { o.sizingMaxSize = "0" }, "maximum size must be positive"},
	} {
		t.Run(name, func(t *testing.T) {
			o := validServeOptions()
			tc.mutate(o)
			_, err := o.controllerConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// kubeClients are fake clients as kube.Client.
type kubeClients struct {
	kubernetes.Interface
	dynamic dynamic.Interface
}

func (c kubeClients) Typed() kubernetes.Interface             { return c.Interface }
func (c kubeClients) Dynamic() dynamic.Interface              { return c.dynamic }
func (c kubeClients) Discovery() discovery.DiscoveryInterface { return c.Interface.Discovery() }

func TestWorkspaceGrantsFailTheStart(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(ecKey)
	require.NoError(t, err)
	clients := kubeClients{Interface: fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-manager-keys", Namespace: "wm"},
		Data: map[string][]byte{
			"grant": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
			"bytes": make([]byte, 32),
		},
	}), dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())}
	connectClients, err := connect.NewClients(nil, kube.Secrets{}, "https://wm.example.com")
	require.NoError(t, err)
	si := &signIns{store: &signin.KubeStore{}, clients: connectClients, baseURL: "https://wm.example.com"}
	ctx := context.Background()
	grants := func(ref string, mutate ...func(*serveOptions)) error {
		o := validServeOptions()
		o.managerNamespace, o.grantSigningKey = "wm", ref
		for _, m := range mutate {
			m(o)
		}
		_, _, err := workspaceGrants(ctx, o, si, clients, nil, nil, slog.Default())
		return err
	}

	require.NoError(t, grants("workspace-manager-keys/grant"))
	a, jwks, err := workspaceGrants(ctx, &serveOptions{}, si, clients, nil, nil, slog.Default())
	require.NoError(t, err)
	assert.True(t, a == nil && jwks == nil, "no signing key, no authorize_workspace and no JWKS")
	assert.ErrorContains(t, grants("workspace-manager-keys"), "must be <secret>/<key>")
	assert.ErrorContains(t, grants("workspace-manager-keys/missing"), `no entry "missing"`)
	assert.ErrorContains(t, grants("other/grant"), "not found")
	assert.ErrorContains(t, grants("workspace-manager-keys/bytes"), "not a PEM private key")
	assert.ErrorContains(t, grants("workspace-manager-keys/grant", func(o *serveOptions) { o.grantTTL = 0 }), "--grant-ttl must be positive")
	assert.ErrorContains(t, grants("workspace-manager-keys/grant", func(o *serveOptions) { o.grantAudience = "" }), "an issuer and an audience")
	assert.ErrorContains(t, grants("workspace-manager-keys/grant", func(o *serveOptions) { o.grantBroker = "" }), "broker name")
}
