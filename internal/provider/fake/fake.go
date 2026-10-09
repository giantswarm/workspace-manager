// Package fake is a second provider kind for tests only: an in-memory store
// configured beside the GitHub instance, so nothing can assume one provider.
// No binary registers it; only tests import it.
package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/oauth2"

	"github.com/giantswarm/workspace-manager/internal/provider"
	"github.com/giantswarm/workspace-manager/internal/provider/providertest"
)

// KindName is the kind's name in the provider configuration.
const KindName = "fake"

// Values are an instance's settings.
type Values struct {
	// Host is the provider's host name; git and API are served on it.
	Host string `json:"host"`
	// Token is the sync credential.
	Token provider.SecretRef `json:"token"`
	// ClientID and ClientSecret are the sign-in's OAuth client.
	ClientID     string             `json:"clientID"`
	ClientSecret provider.SecretRef `json:"clientSecret"`
}

// Store is the provider's data: items per owner, and the token it accepts.
type Store struct {
	mu    sync.Mutex
	items map[string]map[string]provider.Item
	token string
}

// NewStore returns an empty store accepting token.
func NewStore(token string) *Store {
	return &Store{items: map[string]map[string]provider.Item{}, token: token}
}

// Put creates or replaces an item.
func (s *Store) Put(it provider.Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items[it.Owner] == nil {
		s.items[it.Owner] = map[string]provider.Item{}
	}
	it.Topics = append([]string(nil), it.Topics...)
	s.items[it.Owner][it.Name] = it
}

// Delete removes an item.
func (s *Store) Delete(owner, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items[owner], name)
}

// Factory builds fake instances over store.
func Factory(store *Store) provider.Factory {
	return func(raw json.RawMessage) (provider.Kind, error) {
		var v Values
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("values: %w", err)
		}
		if v.Host == "" || v.ClientID == "" {
			return nil, errors.New("values: host and clientID required")
		}
		return &Kind{v: v, store: store}, nil
	}
}

// Kind is a configured fake instance.
type Kind struct {
	v     Values
	store *Store
}

// List implements provider.Kind; it refuses a credential the store does not
// accept, like a provider would.
func (k *Kind) List(_ context.Context, cred oauth2.TokenSource, owner string) ([]provider.Item, error) {
	tok, err := cred.Token()
	if err != nil {
		return nil, err
	}
	k.store.mu.Lock()
	defer k.store.mu.Unlock()
	if tok.AccessToken != k.store.token {
		return nil, errors.New("fake: unauthorized")
	}
	items := make([]provider.Item, 0, len(k.store.items[owner]))
	for _, it := range k.store.items[owner] {
		items = append(items, it)
	}
	return items, nil
}

// SyncCredential implements provider.Kind: the token from its Secret.
func (k *Kind) SyncCredential(ctx context.Context, secrets provider.Secrets, _ string) (oauth2.TokenSource, error) {
	tok, err := secrets.Value(ctx, k.v.Token)
	if err != nil {
		return nil, err
	}
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: string(tok)}), nil
}

// SignIn implements provider.Kind.
func (k *Kind) SignIn() provider.SignIn {
	base := "https://" + k.v.Host
	return provider.SignIn{
		AuthURL:       base + "/oauth/authorize",
		TokenURL:      base + "/oauth/token",
		RevocationURL: base + "/oauth/revoke",
		ClientID:      k.v.ClientID,
		ClientSecret:  k.v.ClientSecret,
		Scopes:        []string{"read", "write"},
	}
}

// Hosts implements provider.Kind.
func (k *Kind) Hosts() provider.Hosts {
	return provider.Hosts{
		Git: []provider.Host{{Name: k.v.Host, Scheme: provider.SchemeBasic, User: "oauth2"}},
		API: []provider.Host{{Name: "api." + k.v.Host, Scheme: provider.SchemeBearer}},
		CLI: "fake",
	}
}

// SecretRefs implements provider.Kind.
func (k *Kind) SecretRefs() []provider.SecretRef {
	return []provider.SecretRef{k.v.Token, k.v.ClientSecret}
}

// Backend is the providertest.Backend of the fake kind.
type Backend struct {
	*Store
}

// NewBackend returns a backend with an empty store.
func NewBackend() *Backend {
	return &Backend{Store: NewStore("fake-sync-token")}
}

// Values implements providertest.Backend.
func (b *Backend) Values() json.RawMessage {
	return json.RawMessage(`{"host":"code.fake.test","token":{"name":"fake-sync","key":"token"},` +
		`"clientID":"fake-client","clientSecret":{"name":"fake-oauth","key":"client-secret"}}`)
}

// Secrets implements providertest.Backend.
func (b *Backend) Secrets() provider.Secrets {
	return providertest.Secrets{{Name: "fake-sync", Key: "token"}: []byte(b.token)}
}
