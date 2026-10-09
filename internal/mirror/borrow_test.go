package mirror

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeAlternates(t *testing.T, clone, line string) {
	t.Helper()
	info := filepath.Join(clone, ".git", "objects", "info")
	require.NoError(t, os.MkdirAll(info, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(info, "alternates"), []byte(line+"\n"), 0o600))
}

func TestBorrowers(t *testing.T) {
	volume := t.TempDir()
	sessions := filepath.Join(volume, SessionsDir)
	// s1 cloned alpha at the Session's own mount of the volume.
	writeAlternates(t, filepath.Join(sessions, "s1", "alpha"), "/workspace/mirrors/acme/alpha.git/objects")
	// s2 cloned alpha under an owner directory with a relative alternates path.
	writeAlternates(t, filepath.Join(sessions, "s2", "acme", "alpha"), "../../../../../../mirrors/acme/alpha.git/objects")
	// s3 borrows from beta only.
	writeAlternates(t, filepath.Join(sessions, "s3", "beta"), "/workspace/mirrors/acme/beta.git/objects")
	// s4 has a clone of its own, without alternates, and a nested one too deep
	// to count.
	require.NoError(t, os.MkdirAll(filepath.Join(sessions, "s4", "own", ".git", "objects"), 0o700))
	writeAlternates(t, filepath.Join(sessions, "s4", "a", "b", "c", "alpha"), "/workspace/mirrors/acme/alpha.git/objects")
	// A dot-prefixed entry is not a session directory.
	writeAlternates(t, filepath.Join(sessions, ".trash", "alpha"), "/workspace/mirrors/acme/alpha.git/objects")

	got, err := borrowers(volume, "mirrors/acme/alpha.git")
	require.NoError(t, err)
	require.Equal(t, []string{"s1", "s2"}, got)

	got, err = borrowers(volume, "mirrors/acme/beta.git")
	require.NoError(t, err)
	require.Equal(t, []string{"s3"}, got)

	got, err = borrowers(volume, "mirrors/acme/gamma.git")
	require.NoError(t, err)
	require.Empty(t, got)

	got, err = borrowers(t.TempDir(), "mirrors/acme/alpha.git")
	require.NoError(t, err)
	require.Empty(t, got, "a volume without sessions has no borrowers")
}
