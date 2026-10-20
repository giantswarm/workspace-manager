// Package muster is the person's provider token as the manager receives it:
// muster's grant. The person grants a provider instance's App once in muster,
// which keeps the grant (one per App per person) and attaches the person's
// token of every App this server's MCPServer names (spec.auth.providerApps) to
// every call, one header per App the person granted. The manager stores no
// token and runs no sign-in; it reads the tokens from the call and names, for
// a provider without one, how the person grants it.
package muster

import (
	"context"
	"fmt"
	"slices"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

// GrantHeaderPrefix starts the header muster carries the person's token of
// one provider App in: X-Muster-Grant-<app>.
const GrantHeaderPrefix = "X-Muster-Grant-" //nolint:gosec // G101: a header name, not a credential

// GrantHeader is the header carrying the person's token of the App app.
func GrantHeader(app string) string { return GrantHeaderPrefix + app }

// StatusGrantRequired is the structuredContent status of a tool result that
// needs the person's grant of the Apps it names in "apps": muster answers it
// auth_required with each App's sign-in link.
const StatusGrantRequired = "grant_required"

// Grants are the provider instances whose App muster holds the people's grants
// of, and this server's registration in muster.
type Grants struct {
	// Apps are the provider instances whose App token muster attaches to
	// every call (the registration's spec.auth.providerApps), each App named
	// after its instance.
	Apps []string
	// Instance is the provider instance whose App the registration is pinned
	// to (spec.auth.authorizationServer), Apps' single-provider predecessor;
	// empty when pinned to none.
	Instance string
	// Server is this server's MCPServer name in muster, which core_auth_login
	// takes for the pinned instance.
	Server string
}

// Token is the person's token for instance on this call, if muster attached
// one.
func (g Grants) Token(ctx context.Context, instance string) (string, bool) {
	return identity.ProviderToken(ctx, instance)
}

// Connected reports whether the person granted instance's App in muster:
// muster attached its token to this call.
func (g Grants) Connected(ctx context.Context, instance string) bool {
	_, ok := g.Token(ctx, instance)
	return ok
}

// App reports whether muster keeps instance's App for this server, so a
// grant_required result naming it is answered with its sign-in link.
func (g Grants) App(instance string) bool {
	return slices.Contains(g.Apps, instance)
}

// SignIn says how the person grants instance's App in muster, or that muster
// holds no grant of it for this server.
func (g Grants) SignIn(instance string) string {
	switch {
	case g.App(instance):
		return fmt.Sprintf("grant the App of provider %s once in muster: core_auth_login app=%s", instance, instance)
	case instance == g.Instance:
		return fmt.Sprintf("grant the App of provider %s once in muster: core_auth_login server=%s", instance, g.Server)
	}
	return fmt.Sprintf("muster holds no grant for provider %s: the workspace-manager's registration in muster names no App of it", instance)
}
