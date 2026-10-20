// Package workspace reads and writes Workspaces for a caller: it checks the
// caller is a member of the Workspace's Organization and that the Workspace
// names only configured provider instances, then writes as the manager's
// ServiceAccount.
//
// Until Organizations get namespaces of their own, every Workspace lives in
// one namespace, where no member is bound to anything; membership is checked
// here, from the groups and verified email of the caller's forwarded identity. When Workspaces
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

// Members are who belongs to an Organization: a caller carrying any of the
// groups, or whose verified email is one of the emails or ends in one of the
// domains. An identity provider that sends no groups claim (Google) admits by
// email or domain only.
type Members struct {
	// Groups are IdP group claims (Dex `groups`).
	Groups []string
	// Emails are member emails, lower-cased.
	Emails []string
	// Domains are member email domains, lower-cased, without the `@`: an
	// email matches its part after the last `@` exactly, no subdomain.
	Domains []string
}

// Organizations maps an Organization to its members. An Organization not
// listed has no members.
type Organizations map[string]Members

// ParseOrganizations reads the Organizations' members: groups
// (`<organization>=<group>[,<group>...]`), emails
// (`<organization>=<email>[,<email>...]`) and email domains
// (`<organization>=<domain>[,<domain>...]`), each list at most once per
// Organization. An Organization needs at least one member of any kind.
func ParseOrganizations(groups, emails, domains []string) (Organizations, error) {
	orgs := Organizations{}
	add := func(kind string, entries []string, valid func(string) bool, set func(*Members, []string)) error {
		seen := map[string]bool{}
		for _, e := range entries {
			name, list, ok := strings.Cut(e, "=")
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				return fmt.Errorf("organization %q: want <organization>=<%s>[,<%s>...]", e, kind, kind)
			}
			if seen[name] {
				return fmt.Errorf("organization %q: %ss listed twice", name, kind)
			}
			seen[name] = true
			var vs []string
			for _, v := range strings.Split(list, ",") {
				if v = strings.TrimSpace(v); v == "" {
					continue
				}
				if !valid(v) {
					return fmt.Errorf("organization %q: %q is no %s", name, v, kind)
				}
				vs = append(vs, v)
			}
			if len(vs) == 0 {
				return fmt.Errorf("organization %q: no member %s", name, kind)
			}
			m := orgs[name]
			set(&m, vs)
			orgs[name] = m
		}
		return nil
	}
	anything := func(string) bool { return true }
	isEmail := func(v string) bool {
		local, domain, ok := strings.Cut(v, "@")
		return ok && local != "" && isDomain(domain)
	}
	if err := add("group", groups, anything, func(m *Members, vs []string) { m.Groups = vs }); err != nil {
		return nil, err
	}
	if err := add("email", emails, isEmail, func(m *Members, vs []string) { m.Emails = lower(vs) }); err != nil {
		return nil, err
	}
	if err := add("domain", domains, isDomain, func(m *Members, vs []string) { m.Domains = lower(vs) }); err != nil {
		return nil, err
	}
	return orgs, nil
}

// isDomain is a plausible email domain: no `@`, no blank, a dot between
// non-empty labels.
func isDomain(v string) bool {
	if strings.ContainsAny(v, "@ \t") {
		return false
	}
	labels := strings.Split(v, ".")
	return len(labels) > 1 && !slices.Contains(labels, "")
}

func lower(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.ToLower(v)
	}
	return out
}

// Check returns nil when the caller in ctx is a member of org, ErrForbidden
// otherwise.
func (o Organizations) Check(ctx context.Context, org string) error {
	id, ok := identity.FromContext(ctx)
	if !ok {
		return fmt.Errorf("%w: no authenticated caller", ErrForbidden)
	}
	if !o.member(id, org) {
		return fmt.Errorf("%w: %s is not a member of Organization %q", ErrForbidden, id, org)
	}
	return nil
}

// Of returns the Organizations the caller in ctx is a member of, sorted,
// whether or not they have a workspace; none for no caller.
func (o Organizations) Of(ctx context.Context) []string {
	out := []string{}
	id, ok := identity.FromContext(ctx)
	if !ok {
		return out
	}
	for org := range o {
		if o.member(id, org) {
			out = append(out, org)
		}
	}
	slices.Sort(out)
	return out
}

// member is the one place membership is decided: the caller carries one of
// org's member groups, or the IdP verified the caller's email and it, or its
// domain, is one of org's.
func (o Organizations) member(id *identity.Identity, org string) bool {
	m := o[org]
	if slices.ContainsFunc(m.Groups, func(g string) bool { return slices.Contains(id.Groups, g) }) {
		return true
	}
	if !id.EmailVerified || id.Email == "" {
		return false
	}
	email := strings.ToLower(id.Email)
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	return slices.Contains(m.Emails, email) || slices.Contains(m.Domains, email[at+1:])
}
