// Package provider is the contract every workspace provider implements, and
// the list of provider instances an installation configures.
//
// A provider fills a workspace's volume from somewhere (GitHub, another git
// host, a file service) and tells the manager what changed since a recorded
// state. Everything a workspace needs from it goes through Kind: the listing,
// the sync credential, the person's sign-in and the hosts the person's token
// is set on at run time. A kind lives in its own package below this one and is
// compiled in by internal/provider/kinds alone, so adding a kind is one
// implementation and that one line, never a change anywhere else.
package provider

import (
	"context"
	"encoding/json"
	"time"

	"golang.org/x/oauth2"
)

// Kind is one provider implementation, configured as one instance. Its four
// parts are what a workspace needs from a provider.
type Kind interface {
	// List resolves an owner (an organization, a user account, a group) to
	// every item it holds that cred can see. Selection by name, language and
	// topics is Select's, over what List reports.
	List(ctx context.Context, cred oauth2.TokenSource, owner string) ([]Item, error)

	// SyncCredential is the provider's own credential for an owner, the one
	// the sync fetches with, built from the instance's Secret references.
	SyncCredential(ctx context.Context, secrets Secrets, owner string) (oauth2.TokenSource, error)

	// SignIn is the person's OAuth 2.0 authorization-code sign-in to the
	// provider.
	SignIn() SignIn

	// Hosts are where the person's token is set at run time, and the CLI the
	// Harness images carry for the provider.
	Hosts() Hosts

	// SecretRefs are every Secret the instance reads, for the start-up
	// validation and the chart's documentation.
	SecretRefs() []SecretRef
}

// Factory builds a Kind from an instance's kind-specific values, refusing
// values it does not know or that are missing.
type Factory func(values json.RawMessage) (Kind, error)

// Item is one thing an owner holds: a repository on a git provider.
type Item struct {
	// Owner is the owner the item was listed under.
	Owner string
	// Name is the item's name within its owner.
	Name string
	// Language is the item's primary language, empty when the provider
	// reports none.
	Language string
	// Topics are the item's topics (labels, tags).
	Topics []string
	// Archived items are read-only at the provider.
	Archived bool
	// Fork is true for an item forked from another.
	Fork bool
	// LastChange is when the item last changed: on git providers the last
	// push to any branch or tag.
	LastChange time.Time
}

// Key identifies an item across owners: `<owner>/<name>`, also its mirror's
// path below `mirrors/` (with `.git`).
func (i Item) Key() string { return i.Owner + "/" + i.Name }

// SecretRef names one key of a Secret in the manager's namespace.
type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

func (r SecretRef) String() string { return r.Name + "/" + r.Key }

// Secrets reads the value a SecretRef names.
type Secrets interface {
	Value(ctx context.Context, ref SecretRef) ([]byte, error)
}

// SignIn is a provider's OAuth 2.0 authorization-code sign-in with PKCE.
type SignIn struct {
	// AuthURL is the authorization endpoint the person is sent to.
	AuthURL string
	// TokenURL redeems the code and refreshes the token.
	TokenURL string
	// RevocationURL revokes a person's grant when they disconnect.
	RevocationURL string
	// ClientID is the OAuth client's public identifier.
	ClientID string
	// ClientSecret is the OAuth client's secret.
	ClientSecret SecretRef
	// Scopes requested; empty where the provider grants by the client's own
	// permissions (a GitHub App).
	Scopes []string
}

// Scheme is how a token is set on a host.
type Scheme string

const (
	// SchemeBasic sets `Authorization: Basic` with Host.User and the token as
	// the password: how git hosts take a token over HTTPS.
	SchemeBasic Scheme = "Basic"
	// SchemeBearer sets `Authorization: Bearer <token>`.
	SchemeBearer Scheme = "Bearer"
)

// Host is one host name the person's token is set on.
type Host struct {
	// Name is the host as in a URL's authority: a host name, with a port only
	// where it is not the scheme's default; no scheme, user or path.
	Name string
	// Scheme is how the token is set.
	Scheme Scheme
	// User is the user name for SchemeBasic; empty for SchemeBearer.
	User string
}

// Hosts are a provider's run-time hosts.
type Hosts struct {
	// Git hosts serve git over HTTPS.
	Git []Host
	// API hosts serve the provider's API, which its CLI calls.
	API []Host
	// CLI is the provider's command-line tool (`gh`, `glab`).
	CLI string
}
