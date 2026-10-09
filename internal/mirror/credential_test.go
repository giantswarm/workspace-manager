package mirror

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServeCredential(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, credentialUsernameFile), []byte("x-access-token\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, credentialTokenFile), []byte("ghs_token\n"), 0o600))
	request := "protocol=https\nhost=github.com\npath=acme/alpha.git\n\n"

	var out bytes.Buffer
	require.NoError(t, ServeCredential(dir, "get", strings.NewReader(request), &out))
	require.Equal(t, "username=x-access-token\npassword=ghs_token\n", out.String())

	for _, op := range []string{"store", "erase"} {
		out.Reset()
		require.NoError(t, ServeCredential(dir, op, strings.NewReader(request), &out))
		require.Empty(t, out.String(), op)
	}

	require.NoError(t, os.Remove(filepath.Join(dir, credentialTokenFile)))
	out.Reset()
	err := ServeCredential(dir, "get", strings.NewReader(request), &out)
	require.Error(t, err)
	require.Empty(t, out.String())
}

func TestCredentialArgs(t *testing.T) {
	args := credentialArgs([]string{"/workspace-manager", "git-credential"}, "/var/run/secrets/workspace-sync/git hub")
	require.Equal(t, []string{
		"-c", "credential.helper=",
		"-c", `credential.helper=!'/workspace-manager' 'git-credential' '/var/run/secrets/workspace-sync/git hub'`,
	}, args)
	require.Equal(t, `'it'\''s'`, shellQuote("it's"))
}
