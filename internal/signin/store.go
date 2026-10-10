// Package signin keeps each person's provider sign-ins: the OAuth 2.0 token
// a person granted a provider instance, sealed at rest, refreshed by the
// manager alone. The refresh token never leaves the manager; callers get
// access tokens, refreshed ahead of expiry so a released token stays valid
// for a turn. A refresh happens once per person and instance across every
// replica, because providers that rotate refresh tokens (GitHub among them)
// reject a second redemption of the same one.
package signin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"golang.org/x/oauth2"
)

// ErrNotSignedIn is returned when a person has no sign-in for an instance.
var ErrNotSignedIn = errors.New("not signed in")

// ErrSignInExpired is returned when a sign-in can no longer be refreshed (no
// refresh token, or the provider refused it): the person signs in again.
var ErrSignInExpired = errors.New("sign-in expired")

// Store keeps sign-ins per person and provider instance. A person is the
// Dex subject; an instance is a provider instance's name.
type Store interface {
	// Get returns the stored token, refresh token included, for the
	// manager's own use; ErrNotSignedIn when there is none.
	Get(ctx context.Context, person, instance string) (*oauth2.Token, error)
	// Put stores a token a sign-in completed with, replacing any earlier one.
	Put(ctx context.Context, person, instance string, token *oauth2.Token) error
	// Delete removes a sign-in; deleting a missing one is no error.
	Delete(ctx context.Context, person, instance string) error
	// AccessToken returns an access token valid for at least the store's
	// refresh margin, refreshing the sign-in when needed.
	AccessToken(ctx context.Context, person, instance string) (string, error)
	// Access is AccessToken with the token's expiry.
	Access(ctx context.Context, person, instance string) (Access, error)
}

// Access is an access token as released to a caller: never the refresh
// token.
type Access struct {
	Token string
	// Expiry is when the token expires; zero when the provider set none.
	Expiry time.Time
}

func accessOf(tok *oauth2.Token) Access {
	return Access{Token: tok.AccessToken, Expiry: tok.Expiry}
}

// OAuth2Configs resolves a provider instance's OAuth 2.0 client: its token
// endpoint, client ID and secret. The store uses it to refresh.
type OAuth2Configs interface {
	OAuth2Config(ctx context.Context, instance string) (*oauth2.Config, error)
}

// OAuth2ConfigFunc adapts a function to OAuth2Configs.
type OAuth2ConfigFunc func(ctx context.Context, instance string) (*oauth2.Config, error)

// OAuth2Config implements OAuth2Configs.
func (f OAuth2ConfigFunc) OAuth2Config(ctx context.Context, instance string) (*oauth2.Config, error) {
	return f(ctx, instance)
}

// personHashLen is the length of the hex person hash in names and labels:
// 160 bits, collision-free in practice, short enough for a label value.
const personHashLen = 40

// PersonHash is the person's Dex subject as it appears in object names and
// labels: a SHA-256 prefix, so nothing in Kubernetes metadata identifies the
// person in clear.
func PersonHash(person string) string {
	sum := sha256.Sum256([]byte(person))
	return hex.EncodeToString(sum[:])[:personHashLen]
}

// objectName is the Secret's and the Lease's name for a sign-in.
func objectName(person, instance string) string {
	return "signin-" + instance + "-" + PersonHash(person)
}
