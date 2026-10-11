package mirror

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// deletedPrefix marks a directory being deleted: it is renamed aside first,
// so neither a Session nor the borrowed-objects rule sees a half-removed
// tree under its name, and a sync removes one an interrupted deletion left.
const deletedPrefix = ".deleted-"

// cleanSessions applies the cleanup to the session directories and returns
// the ones that stay, measured, with the ids of those it deleted. A
// directory goes once its cleanup date has passed: the window after its
// Session's last turn (its creation before the first turn), or after the
// sync that first found it for a Session kagent does not know; and at once
// when kagent lists the workspace's Sessions and no longer lists one it
// listed at the previous sync. Without a cleanup every directory stays.
func (s *Syncer) cleanSessions(c *Cleanup, prev *Manifest, now time.Time) ([]SessionDirectory, []string, error) {
	// The sync lays out sessions/: the runtime creates only a Session's own
	// directory under it and refuses the mount when its parent is missing.
	root := filepath.Join(s.Volume, SessionsDir)
	if err := os.MkdirAll(root, 0o755); err != nil { //nolint:gosec // the runtime creates the sessions' directories under it
		return nil, nil, fmt.Errorf("create sessions directory: %w", err)
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, fmt.Errorf("list session directories: %w", err)
	}
	before := make(map[string]SessionDirectory, len(prev.Sessions))
	for _, d := range prev.Sessions {
		before[d.Session] = d
	}
	var kept []SessionDirectory
	var deleted []string
	for _, d := range dirs {
		name := d.Name()
		if strings.HasPrefix(name, deletedPrefix) {
			s.log().Warn("removing the leftover of an interrupted deletion", "path", filepath.Join(root, name))
			if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
				return nil, nil, fmt.Errorf("remove leftover session directory: %w", err)
			}
			continue
		}
		if !d.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		dir, reason := inventory(name, c, before, now)
		if reason != "" {
			if err := removeAside(root, name); err != nil {
				return nil, nil, err
			}
			deleted = append(deleted, name)
			s.log().Info("session directory deleted", "session", name, "reason", reason, "lastTurn", dir.LastTurn, "cleanupAfter", dir.CleanupAfter)
			continue
		}
		if dir.SizeBytes, err = treeSize(filepath.Join(root, name)); err != nil {
			return nil, nil, err
		}
		kept = append(kept, dir)
	}
	sort.Strings(deleted)
	return kept, deleted, nil
}

// inventory is a directory's record at this sync and, when it goes now, why.
func inventory(name string, c *Cleanup, before map[string]SessionDirectory, now time.Time) (SessionDirectory, string) {
	prev, seen := before[name]
	dir := SessionDirectory{Session: name, FirstSeen: now}
	if seen && !prev.FirstSeen.IsZero() {
		dir.FirstSeen = prev.FirstSeen
	}
	if c == nil {
		// No listing this cycle: what the previous sync knew stands, and
		// nothing goes.
		if seen {
			dir.Name, dir.Creator, dir.LastTurn, dir.Known, dir.CleanupAfter = prev.Name, prev.Creator, prev.LastTurn, prev.Known, prev.CleanupAfter
		}
		return dir, ""
	}
	if sess, ok := c.Sessions[name]; ok {
		dir.Name, dir.Creator, dir.Known = sess.Name, sess.Creator, true
		from := sess.CreatedAt
		if !sess.LastTurn.IsZero() {
			last := sess.LastTurn.UTC()
			dir.LastTurn, from = &last, last
		}
		if from.IsZero() {
			from = dir.FirstSeen
		}
		dir.CleanupAfter = from.Add(c.After()).UTC()
		if !now.Before(dir.CleanupAfter) {
			return dir, "the Session's last turn is past the cleanup window"
		}
		return dir, ""
	}
	if seen && prev.Known {
		dir.Name, dir.Creator, dir.LastTurn = prev.Name, prev.Creator, prev.LastTurn
		dir.CleanupAfter = now
		return dir, "kagent no longer lists the Session: it was deleted"
	}
	dir.CleanupAfter = dir.FirstSeen.Add(c.After()).UTC()
	if !now.Before(dir.CleanupAfter) {
		return dir, "kagent does not know the Session, and the directory is past the cleanup window"
	}
	return dir, ""
}

// removeAside deletes dir/name: renamed aside first, then removed.
func removeAside(dir, name string) error {
	aside := filepath.Join(dir, deletedPrefix+name)
	if err := os.Rename(filepath.Join(dir, name), aside); err != nil {
		return fmt.Errorf("move %s aside for deletion: %w", filepath.Join(dir, name), err)
	}
	if err := os.RemoveAll(aside); err != nil {
		return fmt.Errorf("delete %s: %w", filepath.Join(dir, name), err)
	}
	return nil
}

// removeUnborrowed deletes the dropped mirrors no session directory borrows
// from any more and records the borrowers of those that stay. It returns the
// entries that stay and the keys of the mirrors it deleted.
func (s *Syncer) removeUnborrowed(entries []Entry) ([]Entry, []string, error) {
	var kept []Entry
	var deleted []string
	for _, e := range entries {
		if e.DroppedAt == nil {
			kept = append(kept, e)
			continue
		}
		dir := filepath.Join(s.Volume, filepath.FromSlash(e.Path))
		sessions, err := borrowers(s.Volume, e.Path)
		if err != nil {
			return nil, nil, err
		}
		if len(sessions) > 0 {
			e.BorrowedBy = sessions
			kept = append(kept, e)
			s.log().Info("dropped mirror kept: session directories borrow from it", "repository", e.Key(), "borrowers", sessions)
			continue
		}
		if _, err := os.Stat(dir); err == nil {
			if err := removeAside(filepath.Dir(dir), filepath.Base(dir)); err != nil {
				return nil, nil, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, fmt.Errorf("stat %s: %w", dir, err)
		}
		deleted = append(deleted, e.Key())
		s.log().Info("dropped mirror deleted: no session directory borrows from it", "repository", e.Key())
	}
	return kept, deleted, nil
}

// treeSize measures a session directory: the bytes of its files. A file a
// running Session removes during the walk is skipped.
func treeSize(dir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", dir, err)
	}
	return size, nil
}
