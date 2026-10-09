//go:build envtest

package signin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestSingleFlightRefreshAcrossReplicas runs two stores, standing for two
// replicas, against a real API server: 20 concurrent AccessToken calls on an
// expired sign-in redeem the refresh token exactly once, against a token
// endpoint that refuses a second redemption, and all return the new token.
func TestSingleFlightRefreshAcrossReplicas(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	require.NoError(t, err, "envtest needs KUBEBUILDER_ASSETS (make test-envtest sets it)")
	t.Cleanup(func() { _ = env.Stop() })
	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}, metav1.CreateOptions{})
	require.NoError(t, err)

	// The endpoint answers slowly, so the callers pile up behind the lease.
	ts := newTokenServer(t, 300*time.Millisecond)
	a, logsA := newTestStore(t, client, ts, "replica-a")
	b, logsB := newTestStore(t, client, ts, "replica-b")
	require.NoError(t, a.Put(ctx, testPerson, "github", expired("0")))

	const calls = 20
	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		tokens = make([]string, calls)
		errs   = make([]error, calls)
	)
	for i := range calls {
		store := a
		if i%2 == 1 {
			store = b
		}
		wg.Go(func() {
			<-start
			tokens[i], errs[i] = store.AccessToken(ctx, testPerson, "github")
		})
	}
	close(start)
	wg.Wait()

	for i := range calls {
		require.NoError(t, errs[i], "call %d", i)
		require.Equal(t, "access-1", tokens[i], "call %d", i)
	}
	require.Equal(t, 1, ts.count("refresh-0"), "the refresh token is redeemed exactly once")
	require.Equal(t, 1, ts.total())

	got, err := b.Get(ctx, testPerson, "github")
	require.NoError(t, err)
	require.Equal(t, "refresh-1", got.RefreshToken)
	leases, err := client.CoordinationV1().Leases(testNamespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, leases.Items, "the refresh lease is released")

	// A replica that died holding the lease blocks the refresh only until
	// the lease runs out.
	stale, _ := newTestStore(t, client, ts, "replica-dead", func(o *Options) { o.LeaseDuration = 2 * time.Second })
	held, err := stale.lease.try(ctx, objectName(testPerson, "github"), nil)
	require.NoError(t, err)
	require.True(t, held)
	require.NoError(t, a.Put(ctx, testPerson, "github", expired("1")))
	short, _ := newTestStore(t, client, ts, "replica-c", func(o *Options) { o.LeaseDuration = 2 * time.Second })
	begin := time.Now()
	at, err := short.AccessToken(ctx, testPerson, "github")
	require.NoError(t, err)
	require.Equal(t, "access-2", at)
	require.GreaterOrEqual(t, time.Since(begin), 2*time.Second, "waited for the dead holder's lease")

	logs := logsA.String() + logsB.String()
	require.Contains(t, logs, `msg="sign-in refreshed"`)
	secrets := append(bytesOf("access-0", "refresh-0", "access-1", "refresh-1", "client-secret"), testKey)
	for _, tok := range ts.tokens() {
		secrets = append(secrets, []byte(tok))
	}
	assertNoSecret(t, logs, secrets...)
}
