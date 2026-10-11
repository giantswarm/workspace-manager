package mirror

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGitEnvironHandsTheRootsToGit: the roots the binary's HTTP client
// verifies with reach git, unless git has its own.
func TestGitEnvironHandsTheRootsToGit(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "/etc/workspace-manager/idp-ca/ca.crt")
	t.Setenv("GIT_SSL_CAINFO", "")
	assert.Contains(t, gitEnviron(), "GIT_SSL_CAINFO=/etc/workspace-manager/idp-ca/ca.crt")
	assert.Contains(t, gitEnviron(), "GIT_TERMINAL_PROMPT=0", "the hermetic settings stay")

	t.Setenv("GIT_SSL_CAINFO", "/etc/git/ca.pem")
	env := gitEnviron()
	assert.Contains(t, env, "GIT_SSL_CAINFO=/etc/git/ca.pem", "git's own roots")
	assert.NotContains(t, env, "GIT_SSL_CAINFO=/etc/workspace-manager/idp-ca/ca.crt")

	// t.Setenv restores both at the end; unset, not empty, for this check.
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("GIT_SSL_CAINFO", "")
	os.Unsetenv("SSL_CERT_FILE")
	os.Unsetenv("GIT_SSL_CAINFO")
	for _, e := range gitEnviron() {
		assert.False(t, strings.HasPrefix(e, "GIT_SSL_CAINFO="), "no roots named: %s", e)
	}
}
