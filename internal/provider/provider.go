// Package provider is the contract every workspace provider implements, and
// the list of provider instances an installation configures.
//
// A provider fills a workspace's volume from somewhere (GitHub, another git
// host, a file service) and tells the manager what changed since a recorded
// state. Everything a workspace needs from it goes through Kind: the listing,
// the sync credential, the check of the person's token and the hosts that
// token is set on at run time. The person's token itself is muster's grant,
// never the manager's. A kind lives in its own package below this one and is
// compiled in by internal/provider/kinds alone, so adding a kind is one
// implementation and that one line, never a change anywhere else.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"golang.org/x/oauth2"
)

// ErrCredentialRefused is wrapped by a Kind's error when the provider refused
// the credential itself (revoked, expired, unknown): for the person's token,
// they grant the provider's App in muster again.
var ErrCredentialRefused = errors.New("the provider refused the credential")

// Kind is one provider implementation, configured as one instance. Its parts
// are what a workspace needs from a provider.
type Kind interface {
	// List resolves an owner (an organization, a user account, a group) to
	// every item it holds that cred can see, from a listing at most
	// ListingFreshness old, or made by this call under FreshListing. Selection by name, language and topics is
	// Select's, over what List reports.
	List(ctx context.Context, cred oauth2.TokenSource, owner string) ([]Item, error)

	// Readable returns those of owner's items named in names that the person
	// whose own token cred is can read, each with its last change: the
	// access check at a Session's start. It never answers from a cache, so
	// the answer is the person's access at the time of the call; a name
	// missing from the result is an item the person cannot read. A credential
	// the provider refuses is an error wrapping ErrCredentialRefused.
	Readable(ctx context.Context, cred oauth2.TokenSource, owner string, names []string) ([]Item, error)

	// SyncCredential is the provider's own credential for an owner, the one
	// the sync fetches with, built from the instance's Secret references.
	SyncCredential(ctx context.Context, secrets Secrets, owner string) (oauth2.TokenSource, error)

	// Person is the account the person's token, cred, belongs to at the
	// provider: muster attaches it to every call, and a token the provider
	// refuses is an error wrapping ErrCredentialRefused.
	Person(ctx context.Context, cred oauth2.TokenSource) (string, error)

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

// Clock is the time a kind reads and waits by: its credential's renewal, its
// listing cache and the waits a provider's rate limit asks for. A kind takes
// one so its tests run those without waiting.
type Clock interface {
	Now() time.Time
	// Sleep waits for d, or until ctx ends with its error.
	Sleep(ctx context.Context, d time.Duration) error
}

// RealClock is the wall clock.
type RealClock struct{}

// Now implements Clock.
func (RealClock) Now() time.Time { return time.Now() }

// Sleep implements Clock.
func (RealClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

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
	// SizeKiB is the item's size as the provider reports it, in KiB: the
	// estimate a workspace's volume is sized from.
	SizeKiB int64
	// CloneURL is the git clone URL over http(s), without a credential: the
	// sync's credential helper supplies it.
	CloneURL string
	// DefaultBranch is the branch a mirror's HEAD points at.
	DefaultBranch string
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

// Scheme is how a token is set on a host.
type Scheme string

const (
	// SchemeBasic sets `Authorization: Basic` with Host.User and the token as
	// the password: how git hosts take a token over HTTPS.
	SchemeBasic Scheme = "Basic"
	// SchemeBearer sets `Authorization: Bearer <token>`.
	SchemeBearer Scheme = "Bearer"
)

// Host is one host name, or one path prefix of it, the person's token is
// set on.
type Host struct {
	// Name is the host as in a URL's authority: a host name, with a port only
	// where it is not the scheme's default; no scheme, user or path.
	Name string
	// PathPrefix scopes the entry to the request paths under it: a path
	// starting and ending with `/` (`/api/`), on a host also listed without
	// one. Empty is the whole host.
	PathPrefix string
	// Scheme is how the token is set.
	Scheme Scheme
	// User is the user name for SchemeBasic; empty for SchemeBearer.
	User string
}

// Hosts are a provider's run-time hosts. A host name and path prefix appear
// once: they take one credential. A host serving git and an API that takes
// the git scheme is listed under Git alone; one whose API takes another
// scheme is listed under Git, and its API's path prefix under API.
type Hosts struct {
	// Git hosts serve git over HTTPS, each as a whole host.
	Git []Host
	// API hosts serve the provider's API, which its CLI calls: a host of its
	// own, or a path prefix of a git host.
	API []Host
	// CLI is the provider's command-line tool (`gh`, `glab`).
	CLI string
}
