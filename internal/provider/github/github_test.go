package github_test

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
	"github.com/giantswarm/workspace-manager/internal/provider/github"
	"github.com/giantswarm/workspace-manager/internal/provider/github/githubtest"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
)

func instance(t *testing.T, s *githubtest.Server) provider.Kind {
	t.Helper()
	k, err := github.Factory(github.Options{Clock: s.Clock})(s.Values())
	require.NoError(t, err)
	return k
}

// credential is the owner's sync credential against the stub.
func credential(t *testing.T, k provider.Kind, s *githubtest.Server, owner string) oauth2.TokenSource {
	t.Helper()
	cred, err := k.SyncCredential(context.Background(), s.Secrets(), owner)
	require.NoError(t, err)
	return cred
}

func TestListsAUserAccountAcrossPages(t *testing.T) {
	s := githubtest.NewServer(t)
	s.User("someone")
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		s.Put(provider.Item{Owner: "someone", Name: name, LastChange: time.Now()})
	}
	k := instance(t, s)
	items, err := k.List(context.Background(), credential(t, k, s, "someone"), "someone")
	require.NoError(t, err)
	assert.Len(t, items, 5)
	assert.Equal(t, 3, s.Listed(), "three pages of %d", githubtest.DefaultPageSize)
}

func TestServesHundredsOfRepositoriesFromTheCache(t *testing.T) {
	s := githubtest.NewServer(t)
	s.PageSize = 100
	for i := range 500 {
		s.Put(provider.Item{Owner: "acme", Name: fmt.Sprintf("repo-%03d", i), SizeKiB: int64(i + 1), LastChange: time.Now()})
	}
	k := instance(t, s)
	cred := credential(t, k, s, "acme")

	items, err := k.List(context.Background(), cred, "acme")
	require.NoError(t, err)
	require.Len(t, items, 500)
	for i, it := range items {
		assert.EqualValues(t, i+1, it.SizeKiB, "%s carries its size", it.Name)
	}
	assert.Equal(t, 5, s.Listed(), "five pages of 100")
	assert.Equal(t, 7, s.Requests(), "the installation, its token and five pages")

	for range 20 {
		again, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
		assert.Len(t, again, 500)
	}
	assert.Equal(t, 5, s.Listed(), "served from the cache after one listing")

	s.Clock.Advance(provider.ListingFreshness)
	_, err = k.List(context.Background(), cred, "acme")
	require.NoError(t, err)
	assert.Equal(t, 10, s.Listed(), "listed again once the freshness passed")
	assert.Equal(t, 1, s.Minted(), "on the same installation token")
}

func TestATopicGainedIsSelectedByTheNextListing(t *testing.T) {
	s := githubtest.NewServer(t)
	s.Put(provider.Item{Owner: "acme", Name: "docs", LastChange: time.Now()})
	k := instance(t, s)
	cred := credential(t, k, s, "acme")
	src := provider.Source{Owner: "acme", Topics: []string{"platform"}}

	items, err := k.List(context.Background(), cred, "acme")
	require.NoError(t, err)
	assert.Empty(t, provider.Select(items, src), "no topic yet")

	s.Put(provider.Item{Owner: "acme", Name: "docs", Topics: []string{"platform"}, LastChange: time.Now()})
	items, err = k.List(context.Background(), cred, "acme")
	require.NoError(t, err)
	selected := provider.Select(items, src)
	require.Len(t, selected, 1)
	assert.Equal(t, "acme/docs", selected[0].Key())
}

func TestRenewsTheInstallationTokenBeforeExpiry(t *testing.T) {
	s := githubtest.NewServer(t)
	s.Put(provider.Item{Owner: "acme", Name: "api"})
	k := instance(t, s)
	cred := credential(t, k, s, "acme")
	list := func() {
		t.Helper()
		_, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
	}

	list()
	s.Clock.Advance(30 * time.Minute)
	list()
	assert.Equal(t, 1, s.Minted(), "the hour-long token serves half an hour in")
	s.Clock.Advance(26 * time.Minute)
	list()
	assert.Equal(t, 2, s.Minted(), "renewed five minutes before its expiry")
	assert.Equal(t, 3, s.Listed(), "each listing after the freshness passed went to the API")
}

func TestNotInstalled(t *testing.T) {
	s := githubtest.NewServer(t)
	k := instance(t, s)
	_, err := k.List(context.Background(), credential(t, k, s, "nobody"), "nobody")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the GitHub App 4242 is not installed on nobody")
}

func TestRefusesAnotherOwnersCredential(t *testing.T) {
	s := githubtest.NewServer(t)
	s.Put(provider.Item{Owner: "acme", Name: "api"})
	s.Put(provider.Item{Owner: "umbrella", Name: "theirs"})
	k := instance(t, s)
	_, err := k.List(context.Background(), credential(t, k, s, "umbrella"), "acme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listing acme: the credential is the App's installation on umbrella")
}

func TestMissingPrivateKey(t *testing.T) {
	s := githubtest.NewServer(t)
	k := instance(t, s)
	_, err := k.SyncCredential(context.Background(), providertest.Secrets{}, "acme")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Secret github-app/private-key")
}

func TestRateLimits(t *testing.T) {
	setup := func(t *testing.T) (*githubtest.Server, provider.Kind, oauth2.TokenSource) {
		t.Helper()
		s := githubtest.NewServer(t)
		s.Put(provider.Item{Owner: "acme", Name: "api"})
		k := instance(t, s)
		cred := credential(t, k, s, "acme")
		// The token is minted ahead, so the limits below hit the listing.
		_, err := cred.Token()
		require.NoError(t, err)
		return s, k, cred
	}

	t.Run("a secondary limit's Retry-After is waited out", func(t *testing.T) {
		s, k, cred := setup(t)
		s.LimitNext(1, githubtest.Limit{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"7"}}, Message: "You have exceeded a secondary rate limit."})
		items, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
		assert.Len(t, items, 1)
		assert.Equal(t, []time.Duration{7 * time.Second}, s.Clock.Slept())
	})

	t.Run("the primary limit's reset is waited for", func(t *testing.T) {
		s, k, cred := setup(t)
		reset := s.Clock.Now().Add(42 * time.Second)
		s.LimitNext(1, githubtest.Limit{Status: http.StatusForbidden, Header: http.Header{
			"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(reset.Unix(), 10)},
		}, Message: "API rate limit exceeded for installation ID 1."})
		_, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
		assert.Equal(t, []time.Duration{43 * time.Second}, s.Clock.Slept(), "a second past the reset")
	})

	t.Run("a secondary limit without headers waits a minute", func(t *testing.T) {
		s, k, cred := setup(t)
		s.LimitNext(1, githubtest.Limit{Status: http.StatusForbidden, Message: "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."})
		_, err := k.List(context.Background(), cred, "acme")
		require.NoError(t, err)
		assert.Equal(t, []time.Duration{time.Minute}, s.Clock.Slept())
	})

	t.Run("a reset too far off fails with the limit", func(t *testing.T) {
		s, k, cred := setup(t)
		reset := s.Clock.Now().Add(40 * time.Minute)
		s.LimitNext(1, githubtest.Limit{Status: http.StatusForbidden, Header: http.Header{
			"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(reset.Unix(), 10)},
		}, Message: "API rate limit exceeded for installation ID 1."})
		_, err := k.List(context.Background(), cred, "acme")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "403 Forbidden API rate limit exceeded for installation ID 1. (rate limit resets in 40m1s)")
		assert.Empty(t, s.Clock.Slept())
	})

	t.Run("retries end", func(t *testing.T) {
		s, k, cred := setup(t)
		s.LimitNext(4, githubtest.Limit{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"1"}}, Message: "Slow down."})
		_, err := k.List(context.Background(), cred, "acme")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "429 Too Many Requests Slow down. (rate limit resets in 1s)")
		assert.Len(t, s.Clock.Slept(), 3)
	})

	t.Run("a context cancelled during the wait ends it", func(t *testing.T) {
		s := githubtest.NewServer(t)
		s.Put(provider.Item{Owner: "acme", Name: "api"})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The limited answer cancels the context, before the wait it asks for.
		hc := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := http.DefaultTransport.RoundTrip(r)
			if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
				cancel()
			}
			return resp, err
		})}
		k, err := github.Factory(github.Options{HTTP: hc, Clock: s.Clock})(s.Values())
		require.NoError(t, err)
		cred := credential(t, k, s, "acme")
		_, err = cred.Token()
		require.NoError(t, err)
		s.LimitNext(1, githubtest.Limit{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"30"}}, Message: "Slow down."})
		_, err = k.List(ctx, cred, "acme")
		require.ErrorIs(t, err, context.Canceled)
		assert.Contains(t, err.Error(), "while waiting out a rate limit")
		assert.Empty(t, s.Clock.Slept())
	})

	t.Run("a forbidden answer without a limit is not retried", func(t *testing.T) {
		s, k, cred := setup(t)
		s.LimitNext(1, githubtest.Limit{Status: http.StatusForbidden, Message: "Resource not accessible by integration"})
		_, err := k.List(context.Background(), cred, "acme")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "403 Forbidden Resource not accessible by integration")
		assert.NotContains(t, err.Error(), "rate limit")
		assert.Empty(t, s.Clock.Slept())
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnterpriseServer(t *testing.T) {
	k, err := github.New(github.Values{
		URL:   "https://ghe.example.com/",
		App:   github.AppValues{ID: "1", PrivateKey: provider.SecretRef{Name: "a", Key: "k"}},
		OAuth: github.OAuthValues{ClientID: "c", ClientSecret: provider.SecretRef{Name: "a", Key: "s"}},
	}, github.Options{})
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
	assert.Equal(t, provider.RevokeGrant, s.Revocation)
}

func TestRefusesInvalidValues(t *testing.T) {
	_, err := github.New(github.Values{URL: "ghe.example.com"}, github.Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `url must be an absolute http(s) URL, got "ghe.example.com"`)

	_, err = github.New(github.Values{}, github.Options{})
	require.Error(t, err)
	assert.Equal(t, "values: app.id, app.privateKey.key, app.privateKey.name, oauth.clientID, oauth.clientSecret.key, oauth.clientSecret.name required", err.Error())
}
