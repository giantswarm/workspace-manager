package kube

import (
	"os"
	"path/filepath"
	"testing"

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
