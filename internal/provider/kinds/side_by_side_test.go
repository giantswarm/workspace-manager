package kinds

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/authorize"
	"github.com/giantswarm/workspace-manager/internal/grant"
	"github.com/giantswarm/workspace-manager/internal/mirror"
	"github.com/giantswarm/workspace-manager/internal/muster"
	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/github"
	"github.com/giantswarm/workspace-manager/internal/provider/github/githubtest"
	"github.com/giantswarm/workspace-manager/internal/provider/gitlab"
	"github.com/giantswarm/workspace-manager/internal/provider/gitlab/gitlabtest"
)

// GitHub and GitLab side by side: muster keeps each instance's App and
// attaches the token of every App the person granted. A person who granted
// both is granted a workspace with sources on both in one call, the grant
// naming both providers' hosts; a person who granted one is asked for the
// other only, as grant_required, which muster answers with its sign-in link.
func TestGitHubAndGitLabSideBySide(t *testing.T) {
	gh := githubtest.NewServer(t)
	gh.Put(provider.Item{Owner: "acme", Name: "api", LastChange: authSynced.Add(-time.Hour)})
	ghKind, err := github.Factory(github.Options{Clock: gh.Clock})(gh.Values())
	require.NoError(t, err)
	gl := gitlabtest.NewServer(t)
	gl.Put(provider.Item{Owner: "platform", Name: "charts", LastChange: authSynced.Add(-time.Hour)})
	glKind, err := gitlab.Factory(gitlab.Options{Clock: gl.Clock})(gl.Values())
	require.NoError(t, err)

	cs, keys := authCluster(t, []mirror.Entry{
		{Provider: "github", Owner: "acme", Name: "api", PushedAt: authSynced.Add(-time.Hour)},
		{Provider: "gitlab", Owner: "platform", Name: "charts", PushedAt: authSynced.Add(-time.Hour)},
	})
	a, err := authorize.New(authorize.Config{
		Workspaces: authWorkspaces{"otter": authWorkspace("otter", authReady)},
		Core:       cs,
		Namespace:  authNamespace,
		Instances: []provider.Instance{
			{Name: "github", KindName: github.KindName, Kind: ghKind},
			{Name: "gitlab", KindName: gitlab.KindName, Kind: glKind},
		},
		Grants:         muster.Grants{Apps: []string{"github", "gitlab"}, Server: "workspace-manager"},
		Signer:         &grant.Signer{Keys: keys, Issuer: "https://wm.example.com", Audience: "kagent", TTL: 2 * time.Minute},
		Broker:         "workspace-manager",
		AgentNamespace: agentNamespace,
	})
	require.NoError(t, err)
	tokens := authTokens{
		"alice": {"github": gh.Person("acme/api"), "gitlab": gl.Person("platform/charts")},
		"carol": {"github": gh.Person("acme/api")},
	}

	t.Run("both granted", func(t *testing.T) {
		res, err := a.Authorize(tokens.as("alice"), person("alice"), "otter")
		require.NoError(t, err)
		require.Equal(t, authorize.Granted, res.Outcome, res.Reason)
		assert.Equal(t, []string{"acme/api", "platform/charts"}, res.Repositories)
		hosts := map[string][]string{}
		for _, p := range verified(t, keys, res.Grant).Providers {
			hosts[p.Provider] = append(hosts[p.Provider], p.Hostname)
		}
		for name, kind := range map[string]provider.Kind{"github": ghKind, "gitlab": glKind} {
			for _, h := range kind.Hosts().Git {
				assert.Contains(t, hosts[name], h.Name, "the grant names %s's git host", name)
			}
		}
		for _, token := range tokens["alice"] {
			assert.NotContains(t, res.Grant, token, "no token in the grant")
		}
	})

	t.Run("one granted", func(t *testing.T) {
		res, err := a.Authorize(tokens.as("carol"), person("carol"), "otter")
		require.NoError(t, err)
		assert.Equal(t, authorize.ConnectRequired, res.Outcome)
		assert.Equal(t, []authorize.Connect{{Provider: "gitlab", SignIn: "grant the App of provider gitlab once in muster: core_auth_login app=gitlab"}}, res.Connect)
		assert.Empty(t, res.Grant)

		// The structuredContent muster reads.
		raw, err := json.Marshal(res)
		require.NoError(t, err)
		var structured map[string]any
		require.NoError(t, json.Unmarshal(raw, &structured))
		assert.Equal(t, "grant_required", structured["status"])
		assert.Equal(t, []any{"gitlab"}, structured["apps"])
	})
}
