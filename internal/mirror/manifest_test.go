package mirror

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManifestRoundTrip(t *testing.T) {
	volume := t.TempDir()
	m, err := ReadManifest(volume)
	require.NoError(t, err)
	require.Empty(t, m.Repositories, "a volume before its first sync has an empty manifest")

	dropped := t0
	m = &Manifest{SyncedAt: t1, Repositories: []Entry{
		{Provider: "github", Owner: "acme", Name: "beta", Path: "mirrors/acme/beta.git", Head: "b", PushedAt: t0, SizeBytes: 2, DroppedAt: &dropped},
		{Provider: "github", Owner: "acme", Name: "alpha", Path: "mirrors/acme/alpha.git", Head: "a", PushedAt: t1, SizeBytes: 1},
	}}
	require.NoError(t, m.Write(volume))
	require.NoFileExists(t, filepath.Join(volume, ManifestFile+".tmp"))

	back, err := ReadManifest(volume)
	require.NoError(t, err)
	require.Equal(t, t1, back.SyncedAt)
	require.Equal(t, []string{"acme/alpha", "acme/beta"}, []string{back.Repositories[0].Key(), back.Repositories[1].Key()}, "entries are written in key order")
	require.Nil(t, back.Entry("acme", "alpha").DroppedAt)
	require.Equal(t, t0, *back.Entry("acme", "beta").DroppedAt)
	require.Nil(t, back.Entry("acme", "gamma"))

	raw, err := os.ReadFile(filepath.Join(volume, ManifestFile)) //nolint:gosec // the test's own volume
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"droppedAt": null`)
}

func TestReadSelection(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "selection.json")
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}
	good := `{"repositories":[{"provider":"github","owner":"acme","name":"alpha","cloneUrl":"https://github.com/acme/alpha.git","defaultBranch":"main","pushedAt":"2026-10-09T12:00:00Z"}]}`
	sel, err := ReadSelection(write(good))
	require.NoError(t, err)
	require.Len(t, sel.Repositories, 1)
	require.Equal(t, time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), sel.Repositories[0].PushedAt)

	for name, body := range map[string]string{ //nolint:gosec // the credential-in-URL case is the fixture's point
		"traversal owner":   `{"repositories":[{"provider":"github","owner":"..","name":"alpha","cloneUrl":"https://h/a.git","defaultBranch":"main","pushedAt":"2026-10-09T12:00:00Z"}]}`,
		"separator in name": `{"repositories":[{"provider":"github","owner":"acme","name":"a/b","cloneUrl":"https://h/a.git","defaultBranch":"main","pushedAt":"2026-10-09T12:00:00Z"}]}`,
		"dot-prefixed name": `{"repositories":[{"provider":"github","owner":"acme","name":".tmp-x","cloneUrl":"https://h/a.git","defaultBranch":"main","pushedAt":"2026-10-09T12:00:00Z"}]}`,
		"credential in URL": `{"repositories":[{"provider":"github","owner":"acme","name":"alpha","cloneUrl":"https://u:p@h/a.git","defaultBranch":"main","pushedAt":"2026-10-09T12:00:00Z"}]}`,
		"ssh URL":           `{"repositories":[{"provider":"github","owner":"acme","name":"alpha","cloneUrl":"git@h:a.git","defaultBranch":"main","pushedAt":"2026-10-09T12:00:00Z"}]}`,
		"no default branch": `{"repositories":[{"provider":"github","owner":"acme","name":"alpha","cloneUrl":"https://h/a.git","pushedAt":"2026-10-09T12:00:00Z"}]}`,
		"no pushedAt":       `{"repositories":[{"provider":"github","owner":"acme","name":"alpha","cloneUrl":"https://h/a.git","defaultBranch":"main"}]}`,
		"no provider":       `{"repositories":[{"owner":"acme","name":"alpha","cloneUrl":"https://h/a.git","defaultBranch":"main","pushedAt":"2026-10-09T12:00:00Z"}]}`,
		"listed twice":      good[:len(good)-2] + "," + good[len(`{"repositories":[`):],
		"not json":          `{`,
	} {
		_, err := ReadSelection(write(body))
		require.Error(t, err, name)
	}
	_, err = ReadSelection(filepath.Join(t.TempDir(), "absent.json"))
	require.Error(t, err)
}
