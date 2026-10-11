package kagent_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/giantswarm/workspace-manager/internal/kagent"
	"github.com/giantswarm/workspace-manager/internal/kagent/kagenttest"
)

func TestSessionsWalksEveryPage(t *testing.T) {
	srv := kagenttest.New(t)
	turn := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	srv.Put(kagent.Session{ID: "a", Name: "Fix the flaky test", Creator: "you@example.com", CreatedAt: turn.Add(-time.Hour), LastTurn: turn, Driver: "efs.csi.aws.com", Handle: "fs-1::fsap-1"})
	srv.Put(kagent.Session{ID: "b", Creator: "ana@example.com"})
	srv.Put(kagent.Session{ID: "c"})
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token\n"), 0o600))

	c, err := kagent.Dial(srv.Target, token)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	got, err := c.Sessions(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 3, "three Sessions over two pages")
	require.Equal(t, kagent.Session{ID: "a", Name: "Fix the flaky test", Creator: "you@example.com", CreatedAt: turn.Add(-time.Hour), LastTurn: turn,
		Driver: "efs.csi.aws.com", Handle: "fs-1::fsap-1"}, got[0])
	require.True(t, got[1].LastTurn.IsZero(), "no turn yet")
	require.Empty(t, got[1].Handle, "no workspace volume")
	require.Equal(t, []string{"Bearer sa-token", "Bearer sa-token"}, srv.Bearers(), "every page carries the token")

	// The token is read per call: a rotated one is sent next time.
	require.NoError(t, os.WriteFile(token, []byte("rotated"), 0o600))
	srv.PageSize = 10
	_, err = c.Sessions(context.Background())
	require.NoError(t, err)
	require.Equal(t, "Bearer rotated", srv.Bearers()[2])
}

func TestSessionsFailsWhole(t *testing.T) {
	srv := kagenttest.New(t)
	srv.Fail(status.Error(codes.Unavailable, "down"))
	c, err := kagent.Dial(srv.Target, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.Sessions(context.Background())
	require.ErrorContains(t, err, "down")

	empty := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	c2, err := kagent.Dial(srv.Target, empty)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c2.Close() })
	_, err = c2.Sessions(context.Background())
	require.ErrorContains(t, err, "is empty")
}

func TestParseTarget(t *testing.T) {
	for target, want := range map[string]struct {
		hostPort string
		tls      bool
		err      string
	}{
		"grpc://kagent-controller.kagent:8083": {hostPort: "kagent-controller.kagent:8083"},
		"grpcs://kagent.example.com":           {hostPort: "kagent.example.com:443", tls: true},
		"grpc://kagent-controller.kagent":      {err: "must name a port"},
		"http://kagent:8083":                   {err: "must use the grpc:// or grpcs:// scheme"},
		"grpc://kagent:8083/path":              {err: "with no path"},
	} {
		hostPort, tls, err := kagent.ParseTarget(target)
		if want.err != "" {
			require.ErrorContains(t, err, want.err, target)
			continue
		}
		require.NoError(t, err, target)
		require.Equal(t, want.hostPort, hostPort, target)
		require.Equal(t, want.tls, tls, target)
	}
}
