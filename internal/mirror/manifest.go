package mirror

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ManifestFile is the manifest's name at the volume's root and the key the
// Job's result ConfigMap carries it under.
const ManifestFile = "manifest.json"

// Manifest records what the volume holds after a sync: every mirror with its
// head commit, last-push time and measured size. The next sync compares the
// selection with it, and the controller sizes a provisioned share from it.
type Manifest struct {
	SyncedAt     time.Time `json:"syncedAt"`
	Repositories []Entry   `json:"repositories"`
}

// Entry is one mirror on the volume.
type Entry struct {
	Provider string `json:"provider"`
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	// Path is the mirror's directory relative to the volume's root.
	Path          string `json:"path"`
	CloneURL      string `json:"cloneUrl"`
	DefaultBranch string `json:"defaultBranch"`
	// Head is the commit of the default branch, empty for an empty repository.
	Head      string    `json:"head"`
	PushedAt  time.Time `json:"pushedAt"`
	SizeBytes int64     `json:"sizeBytes"`
	// DroppedAt is set once the repository left the selection; the mirror
	// stays until no session directory borrows from it.
	DroppedAt *time.Time `json:"droppedAt,omitempty"`
}

// Key identifies the entry on the volume: owner/name.
func (e Entry) Key() string { return e.Owner + "/" + e.Name }

// ReadManifest reads the volume's manifest; a volume without one, before its
// first sync, yields an empty manifest.
func ReadManifest(volume string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(volume, ManifestFile)) //nolint:gosec // the volume is the Job's own flag
	if errors.Is(err, fs.ErrNotExist) {
		return &Manifest{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Write replaces the volume's manifest in one rename, so a reader sees the
// previous manifest or the new one, never a partial file.
func (m *Manifest) Write(volume string) error {
	raw, err := m.JSON()
	if err != nil {
		return err
	}
	final := filepath.Join(volume, ManifestFile)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil { //nolint:gosec // the manifest is the volume's shared state
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// JSON renders the manifest with its entries in a stable order.
func (m *Manifest) JSON() ([]byte, error) {
	sort.Slice(m.Repositories, func(i, j int) bool { return m.Repositories[i].Key() < m.Repositories[j].Key() })
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return append(raw, '\n'), nil
}

// Entry returns the entry for owner/name, nil when the manifest has none.
func (m *Manifest) Entry(owner, name string) *Entry {
	for i := range m.Repositories {
		if m.Repositories[i].Owner == owner && m.Repositories[i].Name == name {
			return &m.Repositories[i]
		}
	}
	return nil
}
