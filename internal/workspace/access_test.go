package workspace

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/workspace-manager/internal/identity"
)

func caller(groups ...string) context.Context {
	return identity.ContextWith(context.Background(), &identity.Identity{Subject: "s", Email: "jane@example.com", Groups: groups})
}

func TestParseOrganizations(t *testing.T) {
	orgs, err := ParseOrganizations([]string{"acme=acme-devs, acme-admins", "umbrella = umbrella-devs"})
	require.NoError(t, err)
	require.Equal(t, Organizations{"acme": {"acme-devs", "acme-admins"}, "umbrella": {"umbrella-devs"}}, orgs)

	for _, bad := range [][]string{{"acme"}, {"=g"}, {"acme="}, {"acme=a", "acme=b"}} {
		_, err := ParseOrganizations(bad)
		require.Error(t, err, "%v", bad)
	}
}

func TestCheck(t *testing.T) {
	orgs := Organizations{"acme": {"acme-devs", "acme-admins"}, "nobody": nil}

	require.NoError(t, orgs.Check(caller("other", "acme-admins"), "acme"))

	err := orgs.Check(caller("umbrella-devs"), "acme")
	require.ErrorIs(t, err, ErrForbidden)
	require.ErrorContains(t, err, `jane@example.com is not a member of Organization "acme"`)

	require.ErrorIs(t, orgs.Check(caller("acme-devs"), "nobody"), ErrForbidden, "an Organization without groups has no members")
	require.ErrorIs(t, orgs.Check(caller("acme-devs"), "unknown"), ErrForbidden)
	require.ErrorIs(t, orgs.Check(context.Background(), "acme"), ErrForbidden, "no caller")
}
