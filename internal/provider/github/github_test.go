package github_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/github"
	"github.com/giantswarm/workspace-manager/internal/provider/github/githubtest"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
)

func instance(t *testing.T, s *githubtest.Server) provider.Kind {
	t.Helper()
	k, err := github.Factory(nil)(s.Values())
	require.NoError(t, err)
	return k
}

func TestListsAUserAccountAcrossPages(t *testing.T) {
	s := githubtest.NewServer(t)
	s.User("someone")
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		s.Put(provider.Item{Owner: "someone", Name: name, LastChange: time.Now()})
	}
	k := instance(t, s)
	cred, err := k.SyncCredential(context.Background(), s.Secrets(), "someone")
	require.NoError(t, err)
	items, err := k.List(context.Background(), cred, "someone")
	require.NoError(t, err)
	assert.Len(t, items, 5, "three pages of %d", githubtest.PageSize)
}

func TestReusesTheInstallationToken(t *testing.T) {
	s := githubtest.NewServer(t)
	s.Put(provider.Item{Owner: "acme", Name: "api"})
	k := instance(t, s)
	cred, err := k.SyncCredential(context.Background(), s.Secrets(), "acme")
	require.NoError(t, err)
	for range 3 {
		_, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
	}
	assert.Equal(t, 1, s.Minted(), "one installation token until it nears expiry")
}

func TestNotInstalled(t *testing.T) {
	s := githubtest.NewServer(t)
	k := instance(t, s)
	cred, err := k.SyncCredential(context.Background(), s.Secrets(), "nobody")
	require.NoError(t, err)
	_, err = k.List(context.Background(), cred, "nobody")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the GitHub App 4242 is not installed on nobody")
}

func TestMissingPrivateKey(t *testing.T) {
	s := githubtest.NewServer(t)
	k := instance(t, s)
	_, err := k.SyncCredential(context.Background(), providertest.Secrets{}, "acme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Secret github-app/private-key")
}

func TestEnterpriseServer(t *testing.T) {
	k, err := github.New(github.Values{
		URL:   "https://ghe.example.com/",
		App:   github.AppValues{ID: "1", PrivateKey: provider.SecretRef{Name: "a", Key: "k"}},
		OAuth: github.OAuthValues{ClientID: "c", ClientSecret: provider.SecretRef{Name: "a", Key: "s"}},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, provider.Hosts{
		Git: []provider.Host{{Name: "ghe.example.com", Scheme: provider.SchemeBasic, User: "x-access-token"}},
		API: []provider.Host{{Name: "ghe.example.com", Scheme: provider.SchemeBearer}},
		CLI: "gh",
	}, k.Hosts())
	s := k.SignIn()
	assert.Equal(t, "https://ghe.example.com/login/oauth/authorize", s.AuthURL)
	assert.Equal(t, "https://ghe.example.com/login/oauth/access_token", s.TokenURL)
	assert.Equal(t, "https://ghe.example.com/api/v3/applications/c/grant", s.RevocationURL)
}

func TestRefusesInvalidValues(t *testing.T) {
	_, err := github.New(github.Values{URL: "ghe.example.com"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `url must be an absolute http(s) URL, got "ghe.example.com"`)

	_, err = github.New(github.Values{}, nil)
	require.Error(t, err)
	assert.Equal(t, "values: app.id, app.privateKey.key, app.privateKey.name, oauth.clientID, oauth.clientSecret.key, oauth.clientSecret.name required", err.Error())
}
