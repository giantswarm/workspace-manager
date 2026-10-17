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

	"github.com/giantswarm/workspace-manager/internal/controller"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

func TestLoadProvidersFailsTheStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
providers:
  - name: github
    kind: github
    values:
      app: {id: "1", privateKey: {name: app, key: private-key}}
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

// TestServeControllerFlags: the cycle, the cleanup window (336h, 14 days)
// and the sizing default to the controller's own values and each reads its
// environment variable.
func TestServeControllerFlags(t *testing.T) {
	f := newServeCmd().Flags()
	for name, def := range map[string]string{
		"sync-cycle":               controller.DefaultResync.String(),
		"sessions-cleanup-after":   "336h0m0s", // 14 days, kagent's Session idle TTL on the platform
		"sessions-poll":            controller.DefaultSessionPoll.String(),
		"sizing-factor":            "1.5",
		"sizing-headroom":          "4Gi",
		"sizing-headroom-share":    "0.5",
		"sizing-max-size":          "",
		"sync-job-template":        "",
		"sync-image":               "",
		"leader-election":          "true",
		"leader-election-identity": "",
		"grant-signing-key":        "",
	} {
		flag := f.Lookup(name)
		require.NotNil(t, flag, name)
		assert.Equal(t, def, flag.DefValue, name)
	}

	t.Setenv("WORKSPACE_MANAGER_SYNC_CYCLE", "30m")
	t.Setenv("WORKSPACE_MANAGER_SESSIONS_CLEANUP_AFTER", "168h")
	t.Setenv("WORKSPACE_MANAGER_SESSIONS_POLL", "1m")
	t.Setenv("WORKSPACE_MANAGER_SIZING_FACTOR", "2")
	f = newServeCmd().Flags()
	assert.Equal(t, "30m0s", f.Lookup("sync-cycle").DefValue)
	assert.Equal(t, "168h0m0s", f.Lookup("sessions-cleanup-after").DefValue)
	assert.Equal(t, "1m0s", f.Lookup("sessions-poll").DefValue)
	assert.Equal(t, "2", f.Lookup("sizing-factor").DefValue)
}

func validServeOptions() *serveOptions {
	return &serveOptions{namespace: "kagent", storageClass: "efs-rwx", syncCycle: time.Hour, sessionCleanupAfter: 720 * time.Hour, sessionPoll: time.Minute,
		sizingFactor: 2, sizingHeadroom: "10Gi", sizingHeadroomShare: 0.25, sizingMaxSize: "500Gi",
		syncJobTemplate: "testdata/sync-job.yaml", leaderElection: true, leaderIdentity: "workspace-manager-0",
		grantTTL: 5 * time.Minute, grantAudience: "kagent", grantBroker: "workspace-manager", grantAgentNamespace: "kagent",
		oauthBaseURL: "https://wm.example.com"}
}

func TestServeControllerConfig(t *testing.T) {
	cfg, err := validServeOptions().controllerConfig()
	require.NoError(t, err)
	assert.Equal(t, time.Hour, cfg.Resync)
	assert.Equal(t, 720*time.Hour, cfg.SessionCleanupAfter)
	assert.Equal(t, time.Minute, cfg.SessionPoll)
	assert.Equal(t, "efs-rwx", cfg.StorageClass)
	assert.InDelta(t, 2.0, cfg.Sizing.Factor, 0)
	assert.Equal(t, "10Gi", cfg.Sizing.HeadroomFloor.String())
	assert.InDelta(t, 0.25, cfg.Sizing.HeadroomShare, 0)
	require.NotNil(t, cfg.Sizing.MaxSize)
	assert.Equal(t, "500Gi", cfg.Sizing.MaxSize.String())
	require.NotNil(t, cfg.SyncJobTemplate)
	assert.Equal(t, "workspace-manager-sync", cfg.SyncJobTemplate.Spec.Template.Spec.ServiceAccountName)
	assert.Empty(t, cfg.SyncImage, "no sync image: the template's")
	require.NotNil(t, cfg.LeaderElection)
	assert.Equal(t, "workspace-manager-0", cfg.LeaderElection.Identity)

	o := validServeOptions()
	o.sizingMaxSize, o.leaderElection, o.leaderIdentity = "", false, ""
	cfg, err = o.controllerConfig()
	require.NoError(t, err)
	assert.Nil(t, cfg.Sizing.MaxSize, "empty sets no ceiling")
	assert.Nil(t, cfg.LeaderElection, "off runs the controller in this process")

	o = validServeOptions()
	o.leaderIdentity = ""
	cfg, err = o.controllerConfig()
	require.NoError(t, err)
	host, _ := os.Hostname()
	assert.Equal(t, host, cfg.LeaderElection.Identity, "the host name, the pod's")

	o = validServeOptions()
	o.syncJobTemplate = filepath.Join(t.TempDir(), "job.yaml")
	require.NoError(t, os.WriteFile(o.syncJobTemplate, []byte("spec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers: [{name: sync, image: wm:1}]\n"), 0o600))
	cfg, err = o.controllerConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg.SyncJobTemplate)
	assert.Equal(t, "wm:1", cfg.SyncJobTemplate.Spec.Template.Spec.Containers[0].Image)
	assert.Nil(t, cfg.SyncRoots, "no CA file: the sync fetches with the image's roots")

	o = validServeOptions()
	o.syncJobTemplate, o.syncImage = "testdata/sync-job-no-image.yaml", "workspace-manager:dev"
	cfg, err = o.controllerConfig()
	require.NoError(t, err, "a template without an image runs --sync-image")
	assert.Equal(t, "workspace-manager:dev", cfg.SyncImage)

	o = validServeOptions()
	o.syncCAFile = filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(o.syncCAFile, []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"), 0o600))
	cfg, err = o.controllerConfig()
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n", string(cfg.SyncRoots), "the CA file's bytes travel to the sync Jobs")
}

// TestServeControllerConfigFailsTheStart: a value out of range names its flag.
func TestServeControllerConfigFailsTheStart(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*serveOptions)
		want   string
	}{
		"zero cycle":         {func(o *serveOptions) { o.syncCycle = 0 }, "--sync-cycle"},
		"negative cleanup":   {func(o *serveOptions) { o.sessionCleanupAfter = -time.Hour }, "--sessions-cleanup-after"},
		"zero poll":          {func(o *serveOptions) { o.sessionPoll = 0 }, "--sessions-poll"},
		"bad headroom":       {func(o *serveOptions) { o.sizingHeadroom = "lots" }, "--sizing-headroom"},
		"bad max size":       {func(o *serveOptions) { o.sizingMaxSize = "huge" }, "--sizing-max-size"},
		"factor below one":   {func(o *serveOptions) { o.sizingFactor = 0.5 }, "sizing factor"},
		"negative headroom":  {func(o *serveOptions) { o.sizingHeadroom = "-1Gi" }, "headroom must not be negative"},
		"negative share":     {func(o *serveOptions) { o.sizingHeadroomShare = -1 }, "headroom share"},
		"zero max size":      {func(o *serveOptions) { o.sizingMaxSize = "0" }, "maximum size must be positive"},
		"bad pull policy":    {func(o *serveOptions) { o.syncImagePullPolicy = "Sometimes" }, "--sync-image-pull-policy"},
		"absent template":    {func(o *serveOptions) { o.syncJobTemplate = "/nonexistent/job.yaml" }, "sync job template"},
		"no template":        {func(o *serveOptions) { o.syncJobTemplate = "" }, "--sync-job-template is required"},
		"no template, image": {func(o *serveOptions) { o.syncJobTemplate, o.syncImage = "", "wm:1" }, "--sync-job-template is required"},
		"no image":           {func(o *serveOptions) { o.syncJobTemplate = "testdata/sync-job-no-image.yaml" }, "set --sync-image or the template's image"},
		"absent CA file":     {func(o *serveOptions) { o.syncCAFile = "/nonexistent/ca.crt" }, "--sync-ca-file"},
		"empty CA file":      {func(o *serveOptions) { o.syncCAFile = os.DevNull }, "is empty"},
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
	ctx := context.Background()
	grants := func(ref string, mutate ...func(*serveOptions)) error {
		o := validServeOptions()
		o.managerNamespace, o.grantSigningKey = "wm", ref
		for _, m := range mutate {
			m(o)
		}
		_, _, err := workspaceGrants(ctx, o, muster.Grants{}, clients, nil, nil, slog.Default())
		return err
	}

	require.NoError(t, grants("workspace-manager-keys/grant"))
	assert.ErrorContains(t, grants("workspace-manager-keys"), "must be <secret>/<key>")
	assert.ErrorContains(t, grants("workspace-manager-keys/missing"), `no entry "missing"`)
	assert.ErrorContains(t, grants("other/grant"), "not found")
	assert.ErrorContains(t, grants("workspace-manager-keys/bytes"), "not a PEM private key")
	assert.ErrorContains(t, grants("workspace-manager-keys/grant", func(o *serveOptions) { o.grantTTL = 0 }), "--grant-ttl must be positive")
	assert.ErrorContains(t, grants("workspace-manager-keys/grant", func(o *serveOptions) { o.grantAudience = "" }), "an issuer and an audience")
	assert.ErrorContains(t, grants("workspace-manager-keys/grant", func(o *serveOptions) { o.grantBroker = "" }), "broker name")
	assert.ErrorContains(t, grants("workspace-manager-keys/grant", func(o *serveOptions) { o.grantAgentNamespace = "" }), "Agent namespace")
	assert.ErrorContains(t, grants("workspace-manager-keys/grant", func(o *serveOptions) { o.grantAgentNamespace = "Not/A-Namespace" }), "no namespace name")
}

// TestMusterGrants: --muster-provider pins the registration to a configured
// instance's App; none pins nothing; an unknown instance fails the start.
func TestMusterGrants(t *testing.T) {
	providers := []provider.Instance{{Name: "github", KindName: "github"}, {Name: "gitlab", KindName: "gitlab"}}
	o := &serveOptions{musterServer: "workspace-manager"}

	grants, pin, err := musterGrants(o, providers)
	require.NoError(t, err)
	assert.Nil(t, pin, "no provider named, no pin")
	assert.Empty(t, grants.Instance)

	o.musterProvider = "github"
	grants, pin, err = musterGrants(o, providers)
	require.NoError(t, err)
	require.NotNil(t, pin)
	assert.Equal(t, muster.Grants{Instance: "github", Server: "workspace-manager"}, grants)
	assert.Equal(t, grants, pin.Grants)

	o.musterProvider = "bitbucket"
	_, _, err = musterGrants(o, providers)
	assert.ErrorContains(t, err, `--muster-provider "bitbucket" names no configured provider instance`)

	o.musterProvider, o.musterServer = "github", ""
	_, _, err = musterGrants(o, providers)
	assert.ErrorContains(t, err, "--muster-server")
}

// TestServeMusterFlags: the pin is off by default and the registration's
// name is the chart's.
func TestServeMusterFlags(t *testing.T) {
	f := newServeCmd().Flags()
	assert.Empty(t, f.Lookup("muster-provider").DefValue)
	assert.Equal(t, "workspace-manager", f.Lookup("muster-server").DefValue)
	for _, gone := range []string{"signin-keys-secret", "signin-current-key", "token-exchange-client-id", "token-exchange-client-secret"} {
		assert.Nil(t, f.Lookup(gone), gone)
	}
}
