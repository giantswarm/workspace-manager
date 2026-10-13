package mirror

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// Selection is the sync's input: the repositories the controller resolved for
// the workspace from its sources and filters, with what each provider's
// listing said about them. The controller writes it into the ConfigMap the
// Job mounts.
type Selection struct {
	Repositories []Repository `json:"repositories"`
	// Stopped is a selection that fetches nothing: no mirror is cloned,
	// fetched or dropped, and only the session directories are cleaned up.
	// A deleted workspace's, whose sync stopped, until its last directory
	// is gone; and a live one's when a Session was deleted in kagent.
	Stopped bool `json:"stopped,omitempty"`
	// Cleanup is the session directories' cleanup; nil deletes no session
	// directory and only measures them (the controller could not read
	// kagent's Sessions this cycle, or runs without kagent).
	Cleanup *Cleanup `json:"cleanup,omitempty"`
}

// Cleanup is what the session directories' cleanup works from.
type Cleanup struct {
	// AfterSeconds is how long a directory outlives its Session's last
	// turn; a directory whose Session kagent does not know outlives the
	// sync that first found it as long.
	AfterSeconds int64 `json:"afterSeconds"`
	// Sessions are kagent's Sessions on the workspace's volume, by id, the
	// complete listing: a directory whose Session the previous sync found
	// listed and this one does not lost its Session, and goes at once.
	Sessions map[string]Session `json:"sessions,omitempty"`
	// Deleted is a deleted workspace's cleanup: no Session starts on it any
	// more, so a directory whose Session the listing does not name has none
	// to come back to and goes at once, not after the window.
	Deleted bool `json:"deleted,omitempty"`
}

// Session is a kagent Session as the cleanup reads it.
type Session struct {
	Name      string    `json:"name,omitempty"`
	Creator   string    `json:"creator,omitempty"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	// LastTurn is zero before the Session's first turn.
	LastTurn time.Time `json:"lastTurn,omitzero"`
}

// After is the cleanup window.
func (c *Cleanup) After() time.Duration { return time.Duration(c.AfterSeconds) * time.Second }

// Repository is one selected repository as its provider listed it.
type Repository struct {
	// Provider is the provider instance the repository belongs to; with the
	// owner it names the credential directory the sync fetches with.
	Provider string `json:"provider"`
	// Owner and Name place the mirror at mirrors/<owner>/<name>.git.
	Owner string `json:"owner"`
	Name  string `json:"name"`
	// CloneURL is the provider's clone URL over http(s), without a credential:
	// the credential helper supplies it.
	CloneURL string `json:"cloneUrl"`
	// DefaultBranch is the branch the mirror's HEAD points at and whose head
	// commit the manifest records.
	DefaultBranch string `json:"defaultBranch"`
	// PushedAt is the provider's last-push time of the repository; its mirror
	// is fetched only when it is newer than the manifest's.
	PushedAt time.Time `json:"pushedAt"`
}

// Key identifies the repository on the volume: owner/name.
func (r Repository) Key() string { return r.Owner + "/" + r.Name }

// ReadSelection reads and validates a selection file.
func ReadSelection(path string) (*Selection, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the selection path is the Job's own flag
	if err != nil {
		return nil, fmt.Errorf("read selection: %w", err)
	}
	var sel Selection
	if err := json.Unmarshal(raw, &sel); err != nil {
		return nil, fmt.Errorf("parse selection %s: %w", path, err)
	}
	if err := sel.validate(); err != nil {
		return nil, fmt.Errorf("selection %s: %w", path, err)
	}
	return &sel, nil
}

func (s *Selection) validate() error {
	if s.Cleanup != nil && s.Cleanup.AfterSeconds <= 0 {
		return fmt.Errorf("cleanup: afterSeconds must be positive, got %d", s.Cleanup.AfterSeconds)
	}
	if s.Stopped && len(s.Repositories) > 0 {
		return fmt.Errorf("a stopped selection lists no repositories")
	}
	seen := make(map[string]bool, len(s.Repositories))
	for i, r := range s.Repositories {
		if err := r.validate(); err != nil {
			return fmt.Errorf("repository %d: %w", i, err)
		}
		if seen[r.Key()] {
			return fmt.Errorf("repository %s listed twice", r.Key())
		}
		seen[r.Key()] = true
	}
	return nil
}

func (r Repository) validate() error {
	if r.Provider == "" {
		return fmt.Errorf("%s: provider is empty", r.Key())
	}
	for what, segment := range map[string]string{"owner": r.Owner, "name": r.Name, "provider": r.Provider} {
		if err := validateSegment(segment); err != nil {
			return fmt.Errorf("%s: %s %w", r.Key(), what, err)
		}
	}
	if r.DefaultBranch == "" {
		return fmt.Errorf("%s: default branch is empty", r.Key())
	}
	if r.PushedAt.IsZero() {
		return fmt.Errorf("%s: pushedAt is missing", r.Key())
	}
	u, err := url.Parse(r.CloneURL)
	if err != nil {
		return fmt.Errorf("%s: clone URL: %w", r.Key(), err)
	}
	if u.Scheme != "https" && u.Scheme != "http" || u.Host == "" {
		return fmt.Errorf("%s: clone URL %q is not an http(s) URL", r.Key(), r.CloneURL)
	}
	if u.User != nil {
		return fmt.Errorf("%s: clone URL carries a credential; the credential helper supplies it", r.Key())
	}
	return nil
}

// validateSegment accepts one path segment of the volume's layout: no
// separator, no traversal and no dot-prefixed name, which the sync reserves
// for its temporary directories.
func validateSegment(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("is empty")
	case strings.ContainsAny(s, `/\`):
		return fmt.Errorf("%q contains a path separator", s)
	case strings.HasPrefix(s, "."):
		return fmt.Errorf("%q starts with a dot", s)
	}
	return nil
}
