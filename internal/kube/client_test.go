package kube

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const kubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: lab
  cluster:
    server: https://lab.example.test:6443
- name: other
  cluster:
    server: https://other.example.test:6443
users:
- name: lab
  user: {}
contexts:
- name: lab
  context: {cluster: lab, user: lab}
- name: other
  context: {cluster: other, user: lab}
current-context: lab
`

func TestNewReadsTheKubeconfigAndContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(kubeconfig), 0o600))

	cfg, err := restConfig(Config{Kubeconfig: path})
	require.NoError(t, err)
	assert.Equal(t, "https://lab.example.test:6443", cfg.Host)

	cfg, err = restConfig(Config{Kubeconfig: path, Context: "other"})
	require.NoError(t, err)
	assert.Equal(t, "https://other.example.test:6443", cfg.Host, "the context override wins")

	c, err := New(Config{Kubeconfig: path})
	require.NoError(t, err)
	assert.NotNil(t, c.Dynamic())
	assert.NotNil(t, c.Typed())
	assert.NotNil(t, c.Discovery())
}

func TestNewFailsWithoutAnyCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	_, err := New(Config{Kubeconfig: filepath.Join(t.TempDir(), "missing")})
	assert.Error(t, err)
	_, err = New(Config{InCluster: true})
	assert.Error(t, err, "no pod environment")
}

// A sync cycle's requests leave the default budget room for the tool call a
// person waits on; client-go's own 5 and 10 held it for a second.
func TestTheDefaultBudgetLeavesRoomForAToolCallBehindACycle(t *testing.T) {
	cycle := 60
	limiter := newRateLimiter(Config{})
	assert.InDelta(t, float32(DefaultQPS), limiter.QPS(), 0)
	for range cycle {
		require.True(t, limiter.TryAccept(), "the cycle's requests fit the burst")
	}
	start := time.Now()
	require.NoError(t, limiter.Wait(context.Background()))
	assert.Less(t, time.Since(start), throttledAfter, "the tool call's request does not wait")

	clientGo := newRateLimiter(Config{QPS: 5, Burst: 10})
	for range 10 {
		clientGo.TryAccept()
	}
	start = time.Now()
	require.NoError(t, clientGo.Wait(context.Background()))
	assert.GreaterOrEqual(t, time.Since(start), 150*time.Millisecond, "at 5 per second the next request waits for a token")
}

func TestARequestThatWaitedForTheBudgetIsLogged(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	limiter := newRateLimiter(Config{QPS: 10, Burst: 1, Logger: log})
	require.NoError(t, limiter.Wait(context.Background()))
	assert.Empty(t, logged.String(), "the burst's request does not wait")
	require.NoError(t, limiter.Wait(context.Background()))
	assert.Contains(t, logged.String(), "kubernetes request waited for the client's budget")
	assert.Contains(t, logged.String(), "qps=10")
}

func TestNewRefusesANegativeBudget(t *testing.T) {
	_, err := New(Config{QPS: -1})
	assert.ErrorContains(t, err, "must not be negative")
	_, err = New(Config{Burst: -1})
	assert.ErrorContains(t, err, "must not be negative")
}
