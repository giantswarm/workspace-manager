// Package workspace reads and writes Workspaces for a caller: it checks the
// caller is a member of the Workspace's Organization and that the Workspace
// names only configured provider instances, then writes as the manager's
// ServiceAccount.
//
// Until Organizations get namespaces of their own, every Workspace lives in
// one namespace, where no member is bound to anything; membership is checked
// here, from the groups of the caller's forwarded identity. When Workspaces
// move to the Organization's namespace, the check becomes RBAC on the
// person's identity there, and Organizations goes.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

// ErrForbidden is returned for a caller who is not a member of the
// Workspace's Organization, or for no caller at all.
var ErrForbidden = errors.New("forbidden")

// Organizations maps an Organization to its member groups: a caller carrying
// any of them is a member. An Organization without groups has no members.
type Organizations map[string][]string

// ParseOrganizations reads `<organization>=<group>[,<group>...]` entries, one
// per Organization.
func ParseOrganizations(entries []string) (Organizations, error) {
	orgs := Organizations{}
	for _, e := range entries {
		name, groups, ok := strings.Cut(e, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("organization %q: want <organization>=<group>[,<group>...]", e)
		}
		if _, dup := orgs[name]; dup {
			return nil, fmt.Errorf("organization %q: listed twice", name)
		}
		var gs []string
		for _, g := range strings.Split(groups, ",") {
			if g = strings.TrimSpace(g); g != "" {
				gs = append(gs, g)
			}
		}
		if len(gs) == 0 {
			return nil, fmt.Errorf("organization %q: no member group", name)
		}
		orgs[name] = gs
	}
	return orgs, nil
}

// Check returns nil when the caller in ctx is a member of org, ErrForbidden
// otherwise. It is the one place membership is decided.
func (o Organizations) Check(ctx context.Context, org string) error {
	id, ok := identity.FromContext(ctx)
	if !ok {
		return fmt.Errorf("%w: no authenticated caller", ErrForbidden)
	}
	for _, g := range o[org] {
		if slices.Contains(id.Groups, g) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not a member of Organization %q", ErrForbidden, id, org)
}
