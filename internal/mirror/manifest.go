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

// ErrorKey is the key of the Job's result ConfigMap a failed sync writes its
// error under, beside the previous manifest; a successful sync removes it.
const ErrorKey = "error"

// Manifest records what the volume holds after a sync: every mirror with its
// head commit, last-push time and measured size, and every session directory
// with its Session, size and cleanup date. The next sync compares the
// selection with it, and the controller sizes a provisioned share from it and
// lists the session directories in the Workspace's status.
type Manifest struct {
	SyncedAt     time.Time `json:"syncedAt"`
	Repositories []Entry   `json:"repositories"`
	// Sessions are the session directories the sync left on the volume.
	Sessions []SessionDirectory `json:"sessions,omitempty"`
	// Owners are the owners of the entries under sessions/ the sync, run as
	// another uid, left on the volume: the next cycle's Job looks after them
	// in a container of each.
	Owners []Owner `json:"owners,omitempty"`
}

// SessionDirectory is one directory under sessions/, named by its Session's
// id.
type SessionDirectory struct {
	Session string `json:"session"`
	// Name and Creator are the Session's, as kagent listed it.
	Name    string `json:"name,omitempty"`
	Creator string `json:"creator,omitempty"`
	// LastTurn is the Session's last turn as kagent listed it; nil before
	// its first turn and for a Session kagent does not know.
	LastTurn *time.Time `json:"lastTurn,omitempty"`
	// Known says kagent listed the Session at this sync: a known Session
	// missing from a later listing was deleted, and its directory goes at
	// once.
	Known bool `json:"known,omitempty"`
	// FirstSeen is when a sync first found the directory, from which a
	// directory whose Session kagent does not know is cleaned up.
	FirstSeen time.Time `json:"firstSeen"`
	// SizeBytes is the directory's size as measured by this sync.
	SizeBytes int64 `json:"sizeBytes"`
	// CleanupAfter is when the directory goes: the cleanup window after the
	// Session's last turn (its creation before the first), or after
	// FirstSeen for a Session kagent does not know. Zero when the sync had
	// no cleanup window.
	CleanupAfter time.Time `json:"cleanupAfter,omitzero"`
	// Error is why the sync could neither measure nor delete the directory:
	// it stays, with the size a previous sync measured.
	Error string `json:"error,omitempty"`
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
	// BorrowedBy are the session directories that kept a dropped mirror on
	// the volume at this sync.
	BorrowedBy []string `json:"borrowedBy,omitempty"`
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
	sort.Slice(m.Sessions, func(i, j int) bool { return m.Sessions[i].Session < m.Sessions[j].Session })
	sort.Slice(m.Owners, func(i, j int) bool {
		return m.Owners[i].UID < m.Owners[j].UID || m.Owners[i].UID == m.Owners[j].UID && m.Owners[i].GID < m.Owners[j].GID
	})
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
