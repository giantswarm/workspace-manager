// Package mirror keeps a workspace's repositories on its shared volume: a
// bare mirror of each selected repository under mirrors/<owner>/<name>.git,
// brought up to date by the sync Job with the least work, and the manifest
// recording what the volume holds. Session directories under sessions/ clone
// from the mirrors with git alternates, so a mirror never drops an object such
// a clone may borrow.
package mirror

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	// MirrorsDir holds the bare mirrors, written only by the sync.
	MirrorsDir = "mirrors"
	// SessionsDir holds one directory per Session, written only by it.
	SessionsDir = "sessions"
	// tmpPrefix marks a mirror still being cloned: it is renamed into place
	// once complete, and a sync removes one left over by an interrupted clone.
	tmpPrefix = ".tmp-"
	// DefaultParallel bounds the clones and fetches run at once.
	DefaultParallel = 4
)

// Syncer runs one sync of a workspace's volume.
type Syncer struct {
	// Volume is the mount point of the workspace's volume.
	Volume string
	// CredentialsDir holds one directory per provider instance with the files
	// username and token; a provider without one is fetched anonymously.
	CredentialsDir string
	// CredentialHelper is the command git runs for a credential, given the
	// provider's credential directory and the operation: this binary's
	// git-credential subcommand.
	CredentialHelper []string
	// Git is the git binary; empty looks git up on PATH.
	Git string
	// Parallel bounds the clones and fetches run at once; 0 is DefaultParallel.
	Parallel int
	Log      *slog.Logger
}

// Result is what a sync did, by repository key.
type Result struct {
	Manifest  *Manifest
	Added     []string
	Fetched   []string
	Unchanged []string
	Dropped   []string
	// Removed are manifest entries whose mirror is gone from the volume.
	Removed []string
}

type action int

const (
	actionAdd action = iota
	actionFetch
	actionUnchanged
)

// Run brings the volume's mirrors to the selection: it clones the added
// repositories, fetches those whose last push is newer than the manifest's,
// leaves the others untouched and marks the mirrors of repositories that left
// the selection. A failed clone or fetch fails the run before the manifest is
// written, so the previous manifest stays in place.
func (s *Syncer) Run(ctx context.Context, sel *Selection) (*Result, error) {
	if err := sel.validate(); err != nil {
		return nil, err
	}
	g, err := newGit(s.Git, s.log())
	if err != nil {
		return nil, err
	}
	prev, err := ReadManifest(s.Volume)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(s.Volume, MirrorsDir), 0o755); err != nil { //nolint:gosec // the mirrors are shared with the sessions
		return nil, fmt.Errorf("create mirrors directory: %w", err)
	}
	existing, err := s.listMirrors()
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, len(sel.Repositories))
	actions := make([]action, len(sel.Repositories))
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(s.parallel())
	for i, r := range sel.Repositories {
		prevEntry := prev.Entry(r.Owner, r.Name)
		switch {
		case !existing[r.Key()]:
			actions[i] = actionAdd
		case prevEntry != nil && !r.PushedAt.After(prevEntry.PushedAt):
			actions[i] = actionUnchanged
		default:
			actions[i] = actionFetch
		}
		eg.Go(func() error {
			e, err := s.apply(egCtx, g, actions[i], r, prevEntry)
			if err != nil {
				return err
			}
			entries[i] = e
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	res := &Result{Manifest: &Manifest{SyncedAt: time.Now().UTC().Truncate(time.Second)}}
	for i, r := range sel.Repositories {
		switch actions[i] {
		case actionAdd:
			res.Added = append(res.Added, r.Key())
		case actionFetch:
			res.Fetched = append(res.Fetched, r.Key())
		case actionUnchanged:
			res.Unchanged = append(res.Unchanged, r.Key())
		}
	}
	res.Manifest.Repositories = entries

	selected := make(map[string]bool, len(sel.Repositories))
	for _, r := range sel.Repositories {
		selected[r.Key()] = true
	}
	for key := range existing {
		if selected[key] {
			continue
		}
		e, err := s.dropped(ctx, g, key, prev, res.Manifest.SyncedAt)
		if err != nil {
			return nil, err
		}
		res.Manifest.Repositories = append(res.Manifest.Repositories, e)
		res.Dropped = append(res.Dropped, key)
	}
	for _, e := range prev.Repositories {
		if !existing[e.Key()] && !selected[e.Key()] {
			res.Removed = append(res.Removed, e.Key())
			s.log().Info("mirror gone from the volume", "repository", e.Key())
		}
	}
	for _, list := range []*[]string{&res.Added, &res.Fetched, &res.Unchanged, &res.Dropped, &res.Removed} {
		sort.Strings(*list)
	}
	if err := res.Manifest.Write(s.Volume); err != nil {
		return nil, err
	}
	s.log().Info("sync done", "added", len(res.Added), "fetched", len(res.Fetched), "unchanged", len(res.Unchanged),
		"dropped", len(res.Dropped), "removed", len(res.Removed))
	return res, nil
}

func (s *Syncer) apply(ctx context.Context, g *git, a action, r Repository, prevEntry *Entry) (Entry, error) {
	e := Entry{Provider: r.Provider, Owner: r.Owner, Name: r.Name, Path: mirrorPath(r.Owner, r.Name),
		CloneURL: r.CloneURL, DefaultBranch: r.DefaultBranch, PushedAt: r.PushedAt}
	dir := filepath.Join(s.Volume, filepath.FromSlash(e.Path))
	switch a {
	case actionAdd:
		if err := s.clone(ctx, g, r, dir); err != nil {
			return Entry{}, fmt.Errorf("%s: %w", r.Key(), err)
		}
	case actionFetch:
		if err := s.fetch(ctx, g, r, dir, e.Path); err != nil {
			return Entry{}, fmt.Errorf("%s: %w", r.Key(), err)
		}
	case actionUnchanged:
		e.Head, e.SizeBytes = prevEntry.Head, prevEntry.SizeBytes
		if prevEntry.DefaultBranch == r.DefaultBranch {
			s.log().Info("mirror unchanged", "repository", r.Key())
			return e, nil
		}
		// The default branch moved without a push: HEAD follows it locally.
		if err := g.setHead(ctx, dir, r.DefaultBranch); err != nil {
			return Entry{}, fmt.Errorf("%s: %w", r.Key(), err)
		}
	}
	head, err := g.revParse(ctx, dir, "refs/heads/"+r.DefaultBranch)
	if err != nil {
		return Entry{}, fmt.Errorf("%s: %w", r.Key(), err)
	}
	e.Head = head
	if a != actionUnchanged {
		if e.SizeBytes, err = dirSize(dir); err != nil {
			return Entry{}, fmt.Errorf("%s: %w", r.Key(), err)
		}
	}
	switch a {
	case actionAdd:
		s.log().Info("mirror added", "repository", r.Key(), "head", e.Head, "sizeBytes", e.SizeBytes)
	case actionFetch:
		s.log().Info("mirror fetched", "repository", r.Key(), "head", e.Head, "sizeBytes", e.SizeBytes)
	case actionUnchanged:
		s.log().Info("mirror unchanged, default branch moved", "repository", r.Key(), "head", e.Head)
	}
	return e, nil
}

// clone mirrors the repository into a temporary directory beside its final
// place and renames it there once complete: the volume never holds a partial
// mirror under its name. The mirror's own configuration disables automatic
// garbage collection and expiry, so only the sync's gc step touches objects.
func (s *Syncer) clone(ctx context.Context, g *git, r Repository, dir string) error {
	tmp := filepath.Join(filepath.Dir(dir), tmpPrefix+filepath.Base(dir))
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil { //nolint:gosec // the mirrors are shared with the sessions
		return fmt.Errorf("create owner directory: %w", err)
	}
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("remove leftover clone: %w", err)
	}
	cred, err := s.credentialArgs(r.Provider)
	if err != nil {
		return err
	}
	args := append(cred, "clone", "--mirror", "--quiet",
		"-c", "gc.auto=0", "-c", "gc.pruneExpire=never",
		"--", r.CloneURL, tmp)
	if _, err := g.run(ctx, s.Volume, args...); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := g.setHead(ctx, tmp, r.DefaultBranch); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("move mirror into place: %w", err)
	}
	return nil
}

// fetch brings the mirror up to date and repacks it under the borrowed-objects
// rule.
func (s *Syncer) fetch(ctx context.Context, g *git, r Repository, dir, relPath string) error {
	cred, err := s.credentialArgs(r.Provider)
	if err != nil {
		return err
	}
	// The provider may have moved the repository since the clone.
	if _, err := g.run(ctx, dir, "remote", "set-url", "origin", r.CloneURL); err != nil {
		return err
	}
	if _, err := g.run(ctx, dir, append(cred, "fetch", "--quiet", "--prune", "origin")...); err != nil {
		return err
	}
	if err := g.setHead(ctx, dir, r.DefaultBranch); err != nil {
		return err
	}
	return s.gc(ctx, g, dir, relPath)
}

// gc repacks a fetched mirror. While a session directory's clone borrows the
// mirror's objects, every object stays, reachable or not (a deleted or
// force-pushed branch's commits included); with no borrower the unreachable
// ones are pruned.
func (s *Syncer) gc(ctx context.Context, g *git, dir, relPath string) error {
	sessions, err := borrowers(s.Volume, relPath)
	if err != nil {
		return err
	}
	if len(sessions) > 0 {
		s.log().Info("repacking, keeping unreachable objects", "mirror", relPath, "borrowers", sessions)
		_, err := g.run(ctx, dir, "repack", "-a", "-d", "--keep-unreachable", "--quiet")
		return err
	}
	s.log().Info("repacking, pruning unreachable objects", "mirror", relPath)
	if _, err := g.run(ctx, dir, "repack", "-a", "-d", "--quiet"); err != nil {
		return err
	}
	_, err = g.run(ctx, dir, "prune", "--expire=now")
	return err
}

// dropped records a mirror whose repository left the selection. It keeps the
// previous entry, marked when it was not yet, and measures a mirror the
// manifest never recorded, which an interrupted sync can leave behind.
func (s *Syncer) dropped(ctx context.Context, g *git, key string, prev *Manifest, now time.Time) (Entry, error) {
	owner, name, _ := strings.Cut(key, "/")
	if e := prev.Entry(owner, name); e != nil {
		out := *e
		if out.DroppedAt == nil {
			out.DroppedAt = &now
			s.log().Info("mirror dropped", "repository", key)
		}
		return out, nil
	}
	e := Entry{Owner: owner, Name: name, Path: mirrorPath(owner, name), DroppedAt: &now}
	dir := filepath.Join(s.Volume, filepath.FromSlash(e.Path))
	var err error
	if e.Head, err = g.revParse(ctx, dir, "HEAD"); err != nil {
		return Entry{}, fmt.Errorf("%s: %w", key, err)
	}
	if e.SizeBytes, err = dirSize(dir); err != nil {
		return Entry{}, fmt.Errorf("%s: %w", key, err)
	}
	s.log().Info("mirror dropped, unrecorded by the manifest", "repository", key)
	return e, nil
}

// listMirrors maps the repositories whose mirror directory exists, removing
// the temporary directories an interrupted clone left.
func (s *Syncer) listMirrors() (map[string]bool, error) {
	root := filepath.Join(s.Volume, MirrorsDir)
	owners, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("list mirrors: %w", err)
	}
	out := map[string]bool{}
	for _, owner := range owners {
		if !owner.IsDir() || strings.HasPrefix(owner.Name(), ".") {
			continue
		}
		repos, err := os.ReadDir(filepath.Join(root, owner.Name()))
		if err != nil {
			return nil, fmt.Errorf("list mirrors: %w", err)
		}
		for _, repo := range repos {
			switch {
			case !repo.IsDir():
			case strings.HasPrefix(repo.Name(), tmpPrefix):
				leftover := filepath.Join(root, owner.Name(), repo.Name())
				s.log().Warn("removing the leftover of an interrupted clone", "path", leftover)
				if err := os.RemoveAll(leftover); err != nil {
					return nil, fmt.Errorf("remove leftover clone: %w", err)
				}
			case strings.HasSuffix(repo.Name(), ".git") && !strings.HasPrefix(repo.Name(), "."):
				out[owner.Name()+"/"+strings.TrimSuffix(repo.Name(), ".git")] = true
			}
		}
	}
	return out, nil
}

// credentialArgs are the git options for a provider's credential, none for a
// provider without a credential directory.
func (s *Syncer) credentialArgs(provider string) ([]string, error) {
	dir := filepath.Join(s.CredentialsDir, provider)
	ok, err := hasCredential(dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.log().Info("no credential for provider, fetching anonymously", "provider", provider)
		return nil, nil
	}
	return credentialArgs(s.CredentialHelper, dir), nil
}

func (s *Syncer) parallel() int {
	if s.Parallel <= 0 {
		return DefaultParallel
	}
	return s.Parallel
}

func (s *Syncer) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func mirrorPath(owner, name string) string {
	return path.Join(MirrorsDir, owner, name+".git")
}

// dirSize measures a mirror: the bytes of its files.
func dirSize(dir string) (int64, error) {
	var size int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
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
