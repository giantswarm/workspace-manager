package gitlab_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/gitlab"
	"github.com/giantswarm/workspace-manager/internal/provider/gitlab/gitlabtest"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
)

func instance(t *testing.T, s *gitlabtest.Server) provider.Kind {
	t.Helper()
	k, err := gitlab.Factory(gitlab.Options{Clock: s.Clock})(s.Values())
	require.NoError(t, err)
	return k
}

// credential is the sync credential against the stub.
func credential(t *testing.T, k provider.Kind, s *gitlabtest.Server, owner string) oauth2.TokenSource {
	t.Helper()
	cred, err := k.SyncCredential(context.Background(), s.Secrets(), owner)
	require.NoError(t, err)
	return cred
}

func names(items []provider.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func TestListsSubgroupsAndAUserNamespaceAcrossPages(t *testing.T) {
	s := gitlabtest.NewServer(t)
	for _, name := range []string{"api", "team~web", "team~infra~charts"} {
		s.Put(provider.Item{Owner: "acme", Name: name, Language: "Go", LastChange: s.Clock.Now()})
	}
	s.PutEmpty("acme", "nothing-yet")
	s.User("someone")
	for _, name := range []string{"a", "b", "c"} {
		s.Put(provider.Item{Owner: "someone", Name: name, LastChange: s.Clock.Now()})
	}
	k := instance(t, s)
	cred := credential(t, k, s, "acme")

	items, err := k.List(context.Background(), cred, "acme")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"api", "team~web", "team~infra~charts"}, names(items), "sub-groups' projects, an empty one left out")
	for _, it := range items {
		assert.Equal(t, "acme/"+it.Name, it.Key())
	}
	assert.Equal(t, 2, s.Listed(), "two pages of %d", gitlabtest.DefaultPageSize)
	byName := map[string]provider.Item{}
	for _, it := range items {
		byName[it.Name] = it
	}
	assert.Equal(t, s.URL+"/acme/team/infra/charts.git", byName["team~infra~charts"].CloneURL)
	assert.Equal(t, "Go", byName["team~web"].Language, "the language with the largest share")

	items, err = k.List(context.Background(), cred, "someone")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, names(items), "a user namespace after the group's 404")
}

func TestRefusesASubgroupAsOwner(t *testing.T) {
	s := gitlabtest.NewServer(t)
	k := instance(t, s)
	_, err := k.List(context.Background(), credential(t, k, s, "acme/team"), "acme/team")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "a GitLab owner is a top-level group or a user")
	assert.Zero(t, s.Requests())
}

func TestServesHundredsOfProjectsFromTheCache(t *testing.T) {
	s := gitlabtest.NewServer(t)
	s.PageSize = 100
	for i := range 500 {
		s.Put(provider.Item{Owner: "acme", Name: fmt.Sprintf("repo-%03d", i), Language: "Go", SizeKiB: int64(i + 1), LastChange: s.Clock.Now()})
	}
	k := instance(t, s)
	cred := credential(t, k, s, "acme")

	items, err := k.List(context.Background(), cred, "acme")
	require.NoError(t, err)
	require.Len(t, items, 500)
	for _, it := range items {
		n, _ := strconv.Atoi(it.Name[len("repo-"):])
		assert.EqualValues(t, n+1, it.SizeKiB, "%s carries its size", it.Name)
	}
	assert.Equal(t, 5, s.Listed(), "five pages of 100")
	assert.Equal(t, 500, s.LanguageReads(), "each project's language read once")

	for range 20 {
		again, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
		assert.Len(t, again, 500)
	}
	assert.Equal(t, 5, s.Listed(), "served from the cache after one listing")
	assert.Equal(t, 505, s.Requests(), "no request after the first listing")

	s.Clock.Advance(provider.ListingFreshness)
	s.Put(provider.Item{Owner: "acme", Name: "repo-007", Language: "Rust", SizeKiB: 8, LastChange: s.Clock.Now()})
	items, err = k.List(context.Background(), cred, "acme")
	require.NoError(t, err)
	assert.Equal(t, 10, s.Listed(), "listed again once the freshness passed")
	assert.Equal(t, 501, s.LanguageReads(), "only the project active since asked for its language again")
	for _, it := range items {
		if it.Name == "repo-007" {
			assert.Equal(t, "Rust", it.Language)
		}
	}
}

func TestATopicGainedIsSelectedByTheNextListing(t *testing.T) {
	s := gitlabtest.NewServer(t)
	s.Put(provider.Item{Owner: "acme", Name: "team~docs", LastChange: s.Clock.Now()})
	k := instance(t, s)
	cred := credential(t, k, s, "acme")
	src := provider.Source{Owner: "acme", Topics: []string{"platform"}}

	items, err := k.List(context.Background(), cred, "acme")
	require.NoError(t, err)
	assert.Empty(t, provider.Select(items, src), "no topic yet")

	s.Put(provider.Item{Owner: "acme", Name: "team~docs", Topics: []string{"Platform"}, LastChange: s.Clock.Now()})
	items, err = k.List(context.Background(), cred, "acme")
	require.NoError(t, err)
	selected := provider.Select(items, src)
	require.Len(t, selected, 1)
	assert.Equal(t, "acme/team~docs", selected[0].Key())
}

func TestReadableNamesSubgroupProjects(t *testing.T) {
	s := gitlabtest.NewServer(t)
	s.Put(provider.Item{Owner: "acme", Name: "team~api", LastChange: s.Clock.Now()})
	s.Put(provider.Item{Owner: "acme", Name: "web", LastChange: s.Clock.Now()})
	k := instance(t, s)
	person := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: s.Person("acme/team~api")})
	items, err := k.Readable(context.Background(), person, "acme", []string{"TEAM~API", "web"})
	require.NoError(t, err)
	assert.Equal(t, []string{"team~api"}, names(items))
}

func TestMissingToken(t *testing.T) {
	s := gitlabtest.NewServer(t)
	k := instance(t, s)
	_, err := k.SyncCredential(context.Background(), providertest.Secrets{}, "acme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the group access token (Secret gitlab-sync/token)")
}

func TestRefusedSyncToken(t *testing.T) {
	s := gitlabtest.NewServer(t)
	s.Put(provider.Item{Owner: "acme", Name: "api"})
	k := instance(t, s)
	_, err := k.List(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "revoked"}), "acme")
	require.ErrorIs(t, err, provider.ErrCredentialRefused)
	assert.Contains(t, err.Error(), "listing acme: GET /api/v4/groups/acme/projects: 401 Unauthorized 401 Unauthorized")
}

func TestRateLimits(t *testing.T) {
	setup := func(t *testing.T) (*gitlabtest.Server, provider.Kind, oauth2.TokenSource) {
		t.Helper()
		s := gitlabtest.NewServer(t)
		s.Put(provider.Item{Owner: "acme", Name: "api"})
		k := instance(t, s)
		return s, k, credential(t, k, s, "acme")
	}

	t.Run("Retry-After is waited out", func(t *testing.T) {
		s, k, cred := setup(t)
		s.LimitNext(1, gitlabtest.Limit{Header: http.Header{"Retry-After": {"7"}}})
		items, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
		assert.Len(t, items, 1)
		assert.Equal(t, []time.Duration{7 * time.Second}, s.Clock.Slept())
	})

	t.Run("the limit's reset is waited for", func(t *testing.T) {
		s, k, cred := setup(t)
		reset := s.Clock.Now().Add(42 * time.Second)
		s.LimitNext(1, gitlabtest.Limit{Header: http.Header{"Ratelimit-Reset": {strconv.FormatInt(reset.Unix(), 10)}}})
		_, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
		assert.Equal(t, []time.Duration{43 * time.Second}, s.Clock.Slept(), "a second past the reset")
	})

	t.Run("a limit without headers waits a minute", func(t *testing.T) {
		s, k, cred := setup(t)
		s.LimitNext(1, gitlabtest.Limit{})
		_, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
		assert.Equal(t, []time.Duration{time.Minute}, s.Clock.Slept())
	})

	t.Run("a reset too far off fails with the limit", func(t *testing.T) {
		s, k, cred := setup(t)
		s.LimitNext(1, gitlabtest.Limit{Header: http.Header{"Retry-After": {"2400"}}})
		_, err := k.List(context.Background(), cred, "acme")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "429 Too Many Requests Retry later (rate limit resets in 40m0s)")
		assert.Empty(t, s.Clock.Slept())
	})

	t.Run("retries end", func(t *testing.T) {
		s, k, cred := setup(t)
		s.LimitNext(4, gitlabtest.Limit{Header: http.Header{"Retry-After": {"1"}}})
		_, err := k.List(context.Background(), cred, "acme")
		require.Error(t, err)
		assert.Len(t, s.Clock.Slept(), 3)
	})
}

func TestSelfManaged(t *testing.T) {
	k, err := gitlab.New(gitlab.Values{
		URL:   "https://gitlab.example.com:8443/",
		Token: provider.SecretRef{Name: "gl", Key: "token"},
		OAuth: gitlab.OAuthValues{ClientID: "c", ClientSecret: provider.SecretRef{Name: "gl", Key: "s"}},
	}, gitlab.Options{})
	require.NoError(t, err)
	assert.Equal(t, provider.Hosts{
		Git: []provider.Host{{Name: "gitlab.example.com:8443", Scheme: provider.SchemeBasic, User: "oauth2"}},
		CLI: "glab",
	}, k.Hosts(), "the API under the git host takes the git host's one credential")
	s := k.SignIn()
	assert.Equal(t, provider.SignIn{ //nolint:gosec // G101: URLs and a Secret reference, no credential
		AuthURL:       "https://gitlab.example.com:8443/oauth/authorize",
		TokenURL:      "https://gitlab.example.com:8443/oauth/token",
		RevocationURL: "https://gitlab.example.com:8443/oauth/revoke",
		Revocation:    provider.RevokeRFC7009,
		ClientID:      "c",
		ClientSecret:  provider.SecretRef{Name: "gl", Key: "s"},
		Scopes:        []string{"api", "read_user", "read_repository", "write_repository"},
	}, s)
	assert.Equal(t, []provider.SecretRef{{Name: "gl", Key: "token"}, {Name: "gl", Key: "s"}}, k.SecretRefs())
}

func TestASeparateAPIHostTakesBearer(t *testing.T) {
	k, err := gitlab.New(gitlab.Values{
		URL: "https://git.example.com", APIURL: "https://api.example.com/api/v4",
		Token: provider.SecretRef{Name: "gl", Key: "token"},
		OAuth: gitlab.OAuthValues{ClientID: "c", ClientSecret: provider.SecretRef{Name: "gl", Key: "s"}},
	}, gitlab.Options{})
	require.NoError(t, err)
	assert.Equal(t, []provider.Host{{Name: "api.example.com", Scheme: provider.SchemeBearer}}, k.Hosts().API)
}

func TestDefaultsToGitLabCom(t *testing.T) {
	k, err := gitlab.New(gitlab.Values{
		Token: provider.SecretRef{Name: "gl", Key: "token"},
		OAuth: gitlab.OAuthValues{ClientID: "c", ClientSecret: provider.SecretRef{Name: "gl", Key: "s"}},
	}, gitlab.Options{})
	require.NoError(t, err)
	assert.Equal(t, "gitlab.com", k.Hosts().Git[0].Name)
	assert.Equal(t, "https://gitlab.com/oauth/authorize", k.SignIn().AuthURL)
}

func TestRefusesInvalidValues(t *testing.T) {
	_, err := gitlab.New(gitlab.Values{URL: "gitlab.example.com"}, gitlab.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `url must be an absolute http(s) URL, got "gitlab.example.com"`)

	_, err = gitlab.New(gitlab.Values{}, gitlab.Options{})
	require.Error(t, err)
	assert.Equal(t, "values: oauth.clientID, oauth.clientSecret.key, oauth.clientSecret.name, token.key, token.name required", err.Error())

	_, err = gitlab.Factory(gitlab.Options{})([]byte(`{"app": {}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown field "app"`)
}
