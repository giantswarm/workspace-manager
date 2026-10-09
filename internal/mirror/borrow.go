package mirror

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// borrowDepth bounds the search for clones in a session directory: the
// repository directory, or an owner directory holding it.
const borrowDepth = 2

// borrowers lists the session directories with a clone borrowing the mirror's
// objects: a clone whose git alternates file names the mirror's objects
// directory. A Session sees the volume at its own mount, so the alternates
// path, absolute or relative, is matched by its tail
// mirrors/<owner>/<name>.git/objects.
func borrowers(volume, mirrorPath string) ([]string, error) {
	sessionsDir := filepath.Join(volume, SessionsDir)
	sessions, err := os.ReadDir(sessionsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list session directories: %w", err)
	}
	objects := filepath.FromSlash(path.Join(mirrorPath, "objects"))
	var out []string
	for _, s := range sessions {
		if !s.IsDir() || strings.HasPrefix(s.Name(), ".") {
			continue
		}
		dir := filepath.Join(sessionsDir, s.Name())
		found, err := borrowsFrom(dir, objects)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, s.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// borrowsFrom reports whether a clone under dir has an alternates entry
// ending in objects.
func borrowsFrom(dir, objects string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if found || !d.IsDir() || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.Name() == ".git" {
			alternatesFile := filepath.Join(p, "objects", "info", "alternates")
			raw, readErr := os.ReadFile(alternatesFile) //nolint:gosec // the path is the volume's own
			if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
				return readErr
			}
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				if !filepath.IsAbs(line) {
					line = filepath.Join(p, "objects", line)
				}
				if strings.HasSuffix(filepath.Clean(line), string(filepath.Separator)+objects) {
					found = true
					break
				}
			}
			return filepath.SkipDir
		}
		if strings.HasPrefix(d.Name(), ".") || strings.Count(rel, string(filepath.Separator)) >= borrowDepth {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("scan session directory %s: %w", dir, err)
	}
	return found, nil
}
