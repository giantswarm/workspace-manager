package mirror

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// deletedPrefix marks a directory being deleted: it is renamed aside first,
// so neither a Session nor the borrowed-objects rule sees a half-removed
// tree under its name, and a sync removes one an interrupted deletion left.
const deletedPrefix = ".deleted-"

// sessionsDirMode is sessions/'s mode: the group, the sync pod's fsGroup,
// writes it, so a container of the Job run as a directory's owner renames
// the directory aside; the setgid bit keeps the group on what is created in
// it.
const sessionsDirMode = 0o775 | fs.ModeSetgid

// Owner is the uid and gid of an entry under sessions/ that the sync, run as
// another uid, cannot measure or delete: a runtime creates a Session's
// directory as the Session's user, and a volume that keeps every creator's
// uid and mode (Azure Files over NFS, an NFS server) leaves it to that user
// alone; Amazon EFS's access point maps every uid to its own. The next
// cycle's Job runs the session directories' part of the sync in a container
// of each Owner's (`sync --owner-report`) before its own.
type Owner struct {
	UID int64 `json:"uid"`
	GID int64 `json:"gid"`
}

// String names the owner as `uid:gid`.
func (o Owner) String() string { return fmt.Sprintf("%d:%d", o.UID, o.GID) }

// OwnerReport is what a sync run as an Owner did with the entries under
// sessions/ that Owner owns: the directories it kept, measured, and the ones
// it deleted. The sync of the same Job, which runs after it, takes them into
// its manifest.
type OwnerReport struct {
	Owner    Owner              `json:"owner"`
	Sessions []SessionDirectory `json:"sessions,omitempty"`
	Deleted  []string           `json:"deleted,omitempty"`
}

// ReportFile is an owner's report's name in the Job's reports directory.
func ReportFile(o Owner) string { return fmt.Sprintf("owner-%d-%d.json", o.UID, o.GID) }

// sessionsPass is what one pass over sessions/ found and did.
type sessionsPass struct {
	kept    []SessionDirectory
	deleted []string
	// owners are the owners of the entries this pass left to another uid.
	owners map[Owner]bool
}

// leftTo records an entry's owner when another uid than the sync's, root
// excepted: no container of the sync runs as root.
func (p *sessionsPass) leftTo(o Owner, mine bool) {
	if !mine && o.UID != 0 {
		p.owners[o] = true
	}
}

// cleanSessions applies the cleanup to the session directories this sync
// can look after: every one, or with ownerOnly those owned by the uid it runs
// as. A directory goes once its cleanup date has passed: the window after
// its Session's last turn (its creation before the first turn), or after the
// sync that first found it for a Session kagent does not know; and at once
// when kagent lists the workspace's Sessions and no longer lists one it
// listed at the previous sync, or, on a deleted workspace, does not list its
// Session. Without a cleanup every directory stays. A
// directory an owner's container of the Job reported on is taken as
// reported. One the sync can neither measure nor delete stays with the size
// the previous sync measured and, unless another uid owns it (the next
// cycle's container of that owner looks after it), the reason; it never
// fails the sync.
func (s *Syncer) cleanSessions(c *Cleanup, prev *Manifest, now time.Time, ownerOnly bool) (*sessionsPass, error) {
	root := filepath.Join(s.Volume, SessionsDir)
	if !ownerOnly {
		// The sync lays out sessions/: the runtime creates only a Session's
		// own directory under it and refuses the mount when its parent is
		// missing.
		if err := os.MkdirAll(root, sessionsDirMode); err != nil {
			return nil, fmt.Errorf("create sessions directory: %w", err)
		}
		if err := os.Chmod(root, sessionsDirMode); err != nil && !errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("set the mode of the sessions directory: %w", err)
		}
	}
	reported, reportedDeleted, err := s.readReports()
	if err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir(root)
	if ownerOnly && errors.Is(err, fs.ErrNotExist) {
		return &sessionsPass{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list session directories: %w", err)
	}
	before := make(map[string]SessionDirectory, len(prev.Sessions))
	for _, d := range prev.Sessions {
		before[d.Session] = d
	}
	p := &sessionsPass{deleted: reportedDeleted, owners: map[Owner]bool{}}
	uid := s.uid()
	for _, d := range dirs {
		name := d.Name()
		path := filepath.Join(root, name)
		owner, err := ownerOf(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		mine := owner.UID == int64(uid)
		if ownerOnly && !mine {
			continue
		}
		if strings.HasPrefix(name, deletedPrefix) {
			s.log().Warn("removing the leftover of an interrupted deletion", "path", path)
			if err := os.RemoveAll(path); err != nil {
				s.log().Warn("leftover of an interrupted deletion not removed", "path", path, "owner", owner.String(), "error", err)
				p.leftTo(owner, mine)
			}
			continue
		}
		if !d.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		p.leftTo(owner, mine)
		if r, ok := reported[name]; ok {
			p.kept = append(p.kept, r)
			continue
		}
		dir, reason := inventory(name, c, before, now)
		// What the previous sync measured stands while this one cannot.
		dir.SizeBytes = before[name].SizeBytes
		if reason != "" {
			aside := filepath.Join(root, deletedPrefix+name)
			if err := os.Rename(path, aside); err != nil {
				dir.Error = fmt.Sprintf("move aside for deletion: %v", err)
			} else {
				p.deleted = append(p.deleted, name)
				s.log().Info("session directory deleted", "session", name, "reason", reason, "lastTurn", dir.LastTurn, "cleanupAfter", dir.CleanupAfter)
				if err := os.RemoveAll(aside); err != nil {
					s.log().Warn("deleted session directory not removed; the next sync removes it", "path", aside, "owner", owner.String(), "error", err)
				}
				continue
			}
		}
		if dir.Error == "" {
			if size, err := treeSize(path); err != nil {
				dir.Error = err.Error()
			} else {
				dir.SizeBytes = size
			}
		}
		if dir.Error != "" {
			switch {
			case mine || ownerOnly:
				s.log().Warn("session directory not looked after", "session", name, "owner", owner.String(), "error", dir.Error)
			case owner.UID == 0:
				dir.Error = "owned by root, which no container of the sync runs as: " + dir.Error
				s.log().Warn("session directory not looked after", "session", name, "owner", owner.String(), "error", dir.Error)
			default:
				// The next cycle's Job looks after it as its owner.
				s.log().Info("session directory owned by another uid: the next cycle's sync job looks after it as its owner", "session", name, "owner", owner.String(), "error", dir.Error)
				dir.Error = ""
			}
		}
		p.kept = append(p.kept, dir)
	}
	sort.Strings(p.deleted)
	return p, nil
}

// RunOwner looks after the session directories owned by the uid the sync
// runs as, as cleanSessions does, and returns what it did: the run of the
// Job's container of an Owner's, before the sync's own. It touches nothing
// else on the volume and writes no manifest.
func (s *Syncer) RunOwner(sel *Selection) (*OwnerReport, error) {
	if err := sel.validate(); err != nil {
		return nil, err
	}
	prev, err := ReadManifest(s.Volume)
	if err != nil {
		return nil, err
	}
	p, err := s.cleanSessions(sel.Cleanup, prev, s.now().UTC().Truncate(time.Second), true)
	if err != nil {
		return nil, err
	}
	r := &OwnerReport{Owner: Owner{UID: int64(s.uid()), GID: int64(os.Getgid())}, Sessions: p.kept, Deleted: p.deleted}
	s.log().Info("owner's session directories done", "owner", r.Owner.String(), "sessionDirectories", len(r.Sessions), "sessionDirectoriesCleanedUp", len(r.Deleted))
	return r, nil
}

// readReports reads the owners' reports in ReportsDir: the directories they
// kept, by Session, and the ones they deleted.
func (s *Syncer) readReports() (map[string]SessionDirectory, []string, error) {
	kept := map[string]SessionDirectory{}
	var deleted []string
	if s.ReportsDir == "" {
		return kept, nil, nil
	}
	files, err := filepath.Glob(filepath.Join(s.ReportsDir, "owner-*.json"))
	if err != nil {
		return nil, nil, fmt.Errorf("list the owners' reports: %w", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // the reports directory is the Job's own
		if err != nil {
			return nil, nil, fmt.Errorf("read the owner's report: %w", err)
		}
		var r OwnerReport
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, nil, fmt.Errorf("parse the owner's report %s: %w", f, err)
		}
		for _, d := range r.Sessions {
			kept[d.Session] = d
		}
		deleted = append(deleted, r.Deleted...)
	}
	return kept, deleted, nil
}

// ownerOf is the entry's uid and gid.
func ownerOf(d fs.DirEntry) (Owner, error) {
	info, err := d.Info()
	if err != nil {
		return Owner{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Owner{}, fmt.Errorf("no owner on %s", d.Name())
	}
	return Owner{UID: int64(st.Uid), GID: int64(st.Gid)}, nil
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
	if c.Deleted {
		dir.CleanupAfter = now
		return dir, "the workspace is deleted, and kagent does not list the Session"
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
