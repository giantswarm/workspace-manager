// Package providertest is the contract every provider kind passes: the same
// cases, run against each kind through a Backend that stands in for the
// provider (a stub API server, an in-memory store).
package providertest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/provider"
)

// Backend stands in for one kind's provider.
type Backend interface {
	// Values configure an instance of the kind against the backend.
	Values() json.RawMessage
	// Secrets resolve the instance's Secret references.
	Secrets() provider.Secrets
	// Put creates or replaces an item under its owner.
	Put(item provider.Item)
	// Delete removes an item.
	Delete(owner, name string)
}

// Owner and Other are the owners the cases put items under.
const (
	Owner = "acme"
	Other = "umbrella"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// seed is a mix every case starts from: languages, topics, an archived
// repository and a fork, and one of another owner's.
func seed(b Backend) {
	for _, it := range []provider.Item{
		{Owner: Owner, Name: "api", Language: "Go", Topics: []string{"platform", "backend"}, LastChange: t0},
		{Owner: Owner, Name: "web", Language: "TypeScript", Topics: []string{"platform", "frontend"}, LastChange: t0.Add(time.Hour)},
		{Owner: Owner, Name: "docs", Language: "", Topics: nil, LastChange: t0.Add(2 * time.Hour)},
		{Owner: Owner, Name: "old-api", Language: "Go", Topics: []string{"platform"}, Archived: true, LastChange: t0.Add(-time.Hour)},
		{Owner: Owner, Name: "upstream-fork", Language: "Go", Topics: []string{"backend"}, Fork: true, LastChange: t0.Add(3 * time.Hour)},
		{Owner: Other, Name: "theirs", Language: "Go", Topics: []string{"platform"}, LastChange: t0},
	} {
		b.Put(it)
	}
}

// Run runs the contract against one kind: newBackend starts a fresh backend
// per case, with the factory that builds an instance against it.
func Run(t *testing.T, newBackend func(t *testing.T) (provider.Factory, Backend)) {
	t.Helper()
	setup := func(t *testing.T) (provider.Kind, Backend, func() []provider.Item) {
		factory, b := newBackend(t)
		seed(b)
		k, err := factory(b.Values())
		require.NoError(t, err)
		cred, err := k.SyncCredential(context.Background(), b.Secrets(), Owner)
		require.NoError(t, err)
		list := func() []provider.Item {
			items, err := k.List(context.Background(), cred, Owner)
			require.NoError(t, err)
			return items
		}
		return k, b, list
	}

	t.Run("listing", func(t *testing.T) {
		_, _, list := setup(t)
		items := list()
		byName := map[string]provider.Item{}
		for _, it := range items {
			assert.Equal(t, Owner, it.Owner, "every item is listed under the owner asked for")
			byName[it.Name] = it
		}
		assert.ElementsMatch(t, []string{"api", "web", "docs", "old-api", "upstream-fork"}, keys(byName), "the owner's items, none of another owner's")
		api := byName["api"]
		assert.Equal(t, "Go", api.Language)
		assert.ElementsMatch(t, []string{"platform", "backend"}, api.Topics)
		assert.True(t, api.LastChange.Equal(t0), "last change %s", api.LastChange)
		assert.True(t, byName["old-api"].Archived, "archived reported")
		assert.True(t, byName["upstream-fork"].Fork, "fork reported")
		assert.Empty(t, byName["docs"].Language, "no language reported as empty")
	})

	t.Run("filter semantics", func(t *testing.T) {
		_, _, list := setup(t)
		items := list()
		for _, tc := range []struct {
			name string
			src  provider.Source
			want []string
		}{
			{"no names and no filters take every live repository", provider.Source{}, []string{"api", "docs", "web"}},
			{"names only", provider.Source{Names: []string{"web", "DOCS"}}, []string{"docs", "web"}},
			{"a named archived repository counts", provider.Source{Names: []string{"old-api"}}, []string{"old-api"}},
			{"language is any of the list", provider.Source{Languages: []string{"go", "typescript"}}, []string{"api", "web"}},
			{"topics any", provider.Source{Topics: []string{"frontend", "backend"}}, []string{"api", "web"}},
			{"topics all", provider.Source{Topics: []string{"platform", "backend"}, TopicMatch: provider.TopicsAll}, []string{"api"}},
			{"language and topics both hold", provider.Source{Languages: []string{"Go"}, Topics: []string{"frontend"}}, nil},
			{"archived on request", provider.Source{Languages: []string{"Go"}, IncludeArchived: true}, []string{"api", "old-api"}},
			{"forks on request", provider.Source{Languages: []string{"Go"}, IncludeForks: true}, []string{"api", "upstream-fork"}},
			{"names add to filters", provider.Source{Names: []string{"docs"}, Topics: []string{"frontend"}}, []string{"docs", "web"}},
			{"exclusion wins", provider.Source{Names: []string{"web"}, Topics: []string{"platform"}, Exclude: []string{"Web"}}, []string{"api"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				src := tc.src
				src.Owner = Owner
				var got []string
				for _, it := range provider.Select(items, src) {
					got = append(got, it.Name)
				}
				assert.Equal(t, tc.want, got)
			})
		}
	})

	t.Run("last-change reporting", func(t *testing.T) {
		_, b, list := setup(t)
		src := provider.Source{Owner: Owner, Topics: []string{"platform"}}
		recorded := provider.StateOf(provider.Select(list(), src))
		assert.True(t, provider.Diff(recorded, provider.Select(list(), src)).None(), "an unchanged owner reports no change")

		b.Put(provider.Item{Owner: Owner, Name: "api", Language: "Go", Topics: []string{"platform", "backend"}, LastChange: t0.Add(24 * time.Hour)})
		b.Put(provider.Item{Owner: Owner, Name: "cli", Language: "Go", Topics: []string{"platform"}, LastChange: t0})
		b.Put(provider.Item{Owner: Owner, Name: "web", Language: "TypeScript", Topics: []string{"frontend"}, LastChange: t0.Add(time.Hour)})
		got := provider.Diff(recorded, provider.Select(list(), src))
		assert.Equal(t, provider.Changes{
			Added:   []string{Owner + "/cli"},
			Removed: []string{Owner + "/web"},
			Changed: []string{Owner + "/api"},
		}, got, "a push, a repository joining by topic and one leaving it")

		recorded = provider.StateOf(provider.Select(list(), src))
		b.Delete(Owner, "cli")
		assert.Equal(t, provider.Changes{Removed: []string{Owner + "/cli"}}, provider.Diff(recorded, provider.Select(list(), src)), "a deleted repository leaves")
	})

	t.Run("run-time hosts", func(t *testing.T) {
		k, _, _ := setup(t)
		h := k.Hosts()
		assert.NotEmpty(t, h.Git, "git hosts")
		assert.NotEmpty(t, h.API, "API hosts")
		assert.NotEmpty(t, h.CLI, "the CLI the Harness images carry")
		for _, host := range slices.Concat(h.Git, h.API) {
			assert.NotEmpty(t, host.Name)
			u, err := url.Parse("//" + host.Name)
			assert.True(t, err == nil && u.Host == host.Name && u.User == nil, "%q is a host name with at most a port", host.Name)
			switch host.Scheme {
			case provider.SchemeBasic:
				assert.NotEmpty(t, host.User, "%s: Basic needs a user name", host.Name)
			case provider.SchemeBearer:
				assert.Empty(t, host.User, "%s: Bearer takes no user name", host.Name)
			default:
				t.Errorf("%s: scheme %q is neither Basic nor Bearer", host.Name, host.Scheme)
			}
		}
	})

	t.Run("sign-in", func(t *testing.T) {
		k, _, _ := setup(t)
		s := k.SignIn()
		for name, raw := range map[string]string{"auth": s.AuthURL, "token": s.TokenURL, "revocation": s.RevocationURL} {
			u, err := url.Parse(raw)
			if assert.NoError(t, err, name) {
				assert.True(t, u.IsAbs() && u.Host != "", "%s URL %q is absolute", name, raw)
			}
		}
		assert.NotEmpty(t, s.ClientID)
		assert.Contains(t, k.SecretRefs(), s.ClientSecret, "the client secret is one of the instance's Secret references")
	})
}

// Secrets is an in-memory provider.Secrets.
type Secrets map[provider.SecretRef][]byte

// Value implements provider.Secrets.
func (s Secrets) Value(_ context.Context, ref provider.SecretRef) ([]byte, error) {
	v, ok := s[ref]
	if !ok {
		return nil, fmt.Errorf("secret %s not found", ref)
	}
	return v, nil
}

func keys(m map[string]provider.Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
