package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

func caller(groups ...string) context.Context {
	return identity.ContextWith(context.Background(), &identity.Identity{Subject: "s", Email: "jane@example.com", EmailVerified: true, Groups: groups})
}

// mailer is a caller without groups, as an identity provider that sends no
// groups claim (Google) forwards one.
func mailer(email string, verified bool) context.Context {
	return identity.ContextWith(context.Background(), &identity.Identity{Subject: "s", Email: email, EmailVerified: verified})
}

func TestParseOrganizations(t *testing.T) {
	orgs, err := ParseOrganizations([]string{"acme=acme-devs, acme-admins", "umbrella = umbrella-devs"}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, Organizations{"acme": {Groups: []string{"acme-devs", "acme-admins"}}, "umbrella": {Groups: []string{"umbrella-devs"}}}, orgs)

	for _, bad := range [][]string{{"acme"}, {"=g"}, {"acme="}, {"acme=a", "acme=b"}} {
		_, err := ParseOrganizations(bad, nil, nil)
		require.Error(t, err, "%v", bad)
	}

	t.Run("emails and domains, alone or beside groups", func(t *testing.T) {
		orgs, err := ParseOrganizations(
			[]string{"acme=acme-devs"},
			[]string{"acme=Jane@Example.com", "solo = bob@solo.dev, ann@solo.dev"},
			[]string{"acme=Example.COM", "corp=corp.example"},
		)
		require.NoError(t, err)
		require.Equal(t, Organizations{
			"acme": {Groups: []string{"acme-devs"}, Emails: []string{"jane@example.com"}, Domains: []string{"example.com"}},
			"solo": {Emails: []string{"bob@solo.dev", "ann@solo.dev"}},
			"corp": {Domains: []string{"corp.example"}},
		}, orgs)
	})

	for _, bad := range [][]string{{"acme"}, {"acme="}, {"acme=jane"}, {"acme=@example.com"}, {"acme=jane@"}, {"acme=jane@localhost"}, {"acme=a@x.io", "acme=b@x.io"}} {
		_, err := ParseOrganizations(nil, bad, nil)
		require.Error(t, err, "emails %v", bad)
	}
	for _, bad := range [][]string{{"acme"}, {"acme="}, {"acme=@example.com"}, {"acme=localhost"}, {"acme=example..com"}, {"acme=x.io", "acme=y.io"}} {
		_, err := ParseOrganizations(nil, nil, bad)
		require.Error(t, err, "domains %v", bad)
	}
}

func TestCheck(t *testing.T) {
	orgs := Organizations{"acme": {Groups: []string{"acme-devs", "acme-admins"}}, "nobody": {}}

	require.NoError(t, orgs.Check(caller("other", "acme-admins"), "acme"))

	err := orgs.Check(caller("umbrella-devs"), "acme")
	require.ErrorIs(t, err, ErrForbidden)
	require.ErrorContains(t, err, `jane@example.com is not a member of Organization "acme"`)

	require.ErrorIs(t, orgs.Check(caller("acme-devs"), "nobody"), ErrForbidden, "an Organization without members has none")
	require.ErrorIs(t, orgs.Check(caller("acme-devs"), "unknown"), ErrForbidden)
	require.ErrorIs(t, orgs.Check(context.Background(), "acme"), ErrForbidden, "no caller")
}

func TestCheckByEmail(t *testing.T) {
	orgs := Organizations{
		"acme": {Groups: []string{"acme-devs"}, Domains: []string{"example.com"}},
		"solo": {Emails: []string{"bob@solo.dev"}},
	}

	require.NoError(t, orgs.Check(mailer("jane@example.com", true), "acme"), "a verified email in a member domain")
	require.NoError(t, orgs.Check(mailer("Jane@EXAMPLE.com", true), "acme"), "case-insensitive")
	require.NoError(t, orgs.Check(mailer("bob@solo.dev", true), "solo"), "a member email")
	require.NoError(t, orgs.Check(caller("acme-devs"), "acme"), "groups still count")

	require.ErrorIs(t, orgs.Check(mailer("jane@example.com", false), "acme"), ErrForbidden, "an unverified email never counts")
	require.ErrorIs(t, orgs.Check(mailer("jane@dev.example.com", true), "acme"), ErrForbidden, "no subdomain")
	require.ErrorIs(t, orgs.Check(mailer("jane@notexample.com", true), "acme"), ErrForbidden)
	require.ErrorIs(t, orgs.Check(mailer("ann@solo.dev", true), "solo"), ErrForbidden, "another email of the domain")
	require.ErrorIs(t, orgs.Check(mailer("", true), "acme"), ErrForbidden, "no email")
	require.ErrorIs(t, orgs.Check(mailer("example.com", true), "acme"), ErrForbidden, "no @")
}

func TestOf(t *testing.T) {
	orgs := Organizations{
		"umbrella": {Groups: []string{"umbrella-devs", "staff"}},
		"acme":     {Groups: []string{"acme-devs", "staff"}, Domains: []string{"acme.example"}},
		"nobody":   {},
	}

	require.Equal(t, []string{"acme"}, orgs.Of(caller("other", "acme-devs")))
	require.Equal(t, []string{"acme", "umbrella"}, orgs.Of(caller("staff")), "sorted")
	require.Equal(t, []string{}, orgs.Of(caller("other")))
	require.Equal(t, []string{"acme"}, orgs.Of(mailer("jane@acme.example", true)), "by domain")
	require.Equal(t, []string{}, orgs.Of(context.Background()), "no caller")
}
