package kinds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/fake"
	"github.com/giantswarm/workspace-manager/internal/provider/github"
	"github.com/giantswarm/workspace-manager/internal/provider/github/githubtest"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
)

// testRegistry is the binary's registry plus the fake kind.
func testRegistry(store *fake.Store) provider.Registry {
	r := Registry(nil)
	r[fake.KindName] = fake.Factory(store)
	return r
}

// backends stand in for every kind's provider, each with the factory that
// builds an instance against it; a registered kind without one fails
// TestContract.
var backends = map[string]func(t *testing.T) (provider.Factory, providertest.Backend){
	github.KindName: func(t *testing.T) (provider.Factory, providertest.Backend) {
		s := githubtest.NewServer(t)
		return github.Factory(github.Options{Clock: s.Clock}), s
	},
	fake.KindName: func(*testing.T) (provider.Factory, providertest.Backend) {
		b := fake.NewBackend()
		return fake.Factory(b.Store), b
	},
}

func TestContract(t *testing.T) {
	for _, kind := range testRegistry(nil).Kinds() {
		t.Run(kind, func(t *testing.T) {
			newBackend, ok := backends[kind]
			require.True(t, ok, "kind %s has no contract backend", kind)
			providertest.Run(t, newBackend)
		})
	}
}

const twoProviders = `
providers:
  - name: github
    kind: github
    values:
      app:
        id: "4242"
        privateKey: {name: github-app, key: private-key}
      oauth:
        clientID: Iv1.example
        clientSecret: {name: github-oauth, key: client-secret}
  - name: other
    kind: fake
    values:
      host: code.example.test
      token: {name: other-sync, key: token}
      clientID: other-client
      clientSecret: {name: other-oauth, key: client-secret}
`

func TestLoadTwoKinds(t *testing.T) {
	instances, err := testRegistry(fake.NewStore("t")).Load([]byte(twoProviders))
	require.NoError(t, err)
	require.Len(t, instances, 2)
	assert.Equal(t, "github", instances[0].Name)
	assert.Equal(t, github.KindName, instances[0].KindName)
	assert.Equal(t, "github.com", instances[0].Hosts().Git[0].Name)
	assert.Equal(t, "api.github.com", instances[0].Hosts().API[0].Name)
	assert.Equal(t, "other", instances[1].Name)
	assert.Equal(t, fake.KindName, instances[1].KindName)
	assert.Equal(t, "code.example.test", instances[1].Hosts().Git[0].Name)
}

func TestLoadRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, config, want string
	}{
		{"duplicate name", twoProviders + `
  - name: github
    kind: fake
    values: {host: x.test, clientID: c}
`, `provider "github": duplicate name`},
		{"unknown kind", `
providers:
  - name: lab
    kind: gitlab
`, `provider "lab": unknown kind "gitlab" (known: fake, github)`},
		{"missing name", `
providers:
  - kind: github
`, `provider 1: name is required`},
		{"name not a DNS label", `
providers:
  - name: GitHub_Main
    kind: github
`, `provider "GitHub_Main": name must be a DNS label`},
		{"missing Secret reference", `
providers:
  - name: github
    kind: github
    values:
      app: {id: "1", privateKey: {name: github-app}}
      oauth: {clientID: c, clientSecret: {name: s, key: k}}
`, `provider "github" (kind github): values: app.privateKey.key required`},
		{"unknown value", `
providers:
  - name: github
    kind: github
    values: {token: x}
`, `provider "github" (kind github): values: json: unknown field "token"`},
		{"unknown instance field", `
providers:
  - name: github
    kind: github
    github: {}
`, `unknown field "github"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testRegistry(fake.NewStore("t")).Load([]byte(tc.config))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLoadEmpty(t *testing.T) {
	for _, config := range []string{"", "providers: []", "providers:"} {
		instances, err := Registry(nil).Load([]byte(config))
		require.NoError(t, err, "%q", config)
		assert.Empty(t, instances)
	}
}

func TestBinaryServesNoFake(t *testing.T) {
	assert.Equal(t, []string{github.KindName}, Registry(nil).Kinds())
}
