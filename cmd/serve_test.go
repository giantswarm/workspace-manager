package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
