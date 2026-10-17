// Package muster is the person's provider token as the manager receives it:
// muster's grant. The person grants a provider's App once in muster, which
// keeps the grant (one per App per person) and attaches the person's App
// token to every call of this server's MCPServer, pinned to that App. The
// manager stores no token and runs no sign-in; it reads the token from the
// call and names, for a provider without one, how the person grants it.
package muster

import (
	"context"
	"fmt"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

// Grants are the provider instance whose App muster pins this server's
// registration to, and that registration's name in muster.
type Grants struct {
	// Instance is the provider instance whose App muster holds the people's
	// grants of; empty when the registration is pinned to none.
	Instance string
	// Server is this server's MCPServer name in muster, which core_auth_login
	// takes.
	Server string
}

// Token is the person's token for instance on this call: the one muster
// attached, for the pinned instance only.
func (g Grants) Token(ctx context.Context, instance string) (string, bool) {
	return identity.ProviderToken(ctx, instance)
}

// Connected reports whether the person granted instance's App in muster:
// muster attached its token to this call.
func (g Grants) Connected(ctx context.Context, instance string) bool {
	_, ok := g.Token(ctx, instance)
	return ok
}

// SignIn says how the person grants instance's App: in muster for the pinned
// instance, and that muster holds no grant of any other.
func (g Grants) SignIn(instance string) string {
	if g.Instance != "" && instance == g.Instance {
		return fmt.Sprintf("grant the App of provider %s once in muster: core_auth_login server=%s", instance, g.Server)
	}
	if g.Instance == "" {
		return fmt.Sprintf("muster holds no grant for provider %s: this installation pins the workspace-manager to no provider's App", instance)
	}
	return fmt.Sprintf("muster holds no grant for provider %s: the workspace-manager is pinned to the App of provider %s only", instance, g.Instance)
}
