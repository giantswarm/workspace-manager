package workspace

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/giantswarm/workspace-manager/api/v1alpha1"
	"github.com/giantswarm/workspace-manager/internal/provider"
)

// ErrUnknownProvider is returned for a provider instance the installation
// does not configure.
var ErrUnknownProvider = errors.New("unknown provider instance")

// Listings reads an owner's items with the provider instance's own sync
// credential, the listing a workspace is resolved from, served from the
// kind's listing cache. Nothing about a caller's own access enters it: who
// edits a workspace and who uses it may be different people, and the person's
// access is checked at a Session's start.
type Listings struct {
	instances map[string]provider.Instance
	names     []string
	secrets   provider.Secrets
}

// NewListings returns the listings of instances, their credentials read
// through secrets.
func NewListings(instances []provider.Instance, secrets provider.Secrets) *Listings {
	l := &Listings{instances: make(map[string]provider.Instance, len(instances)), secrets: secrets}
	for _, in := range instances {
		l.instances[in.Name] = in
		l.names = append(l.names, in.Name)
	}
	return l
}

// List returns owner's items on the provider instance named instance.
func (l *Listings) List(ctx context.Context, instance, owner string) ([]provider.Item, error) {
	in, ok := l.instances[instance]
	if !ok {
		return nil, fmt.Errorf("%w %q (configured: %s)", ErrUnknownProvider, instance, known(l.names))
	}
	cred, err := in.SyncCredential(ctx, l.secrets, owner)
	if err != nil {
		return nil, fmt.Errorf("the %s credential for %s: %w", instance, owner, err)
	}
	items, err := in.List(ctx, cred, owner)
	if err != nil {
		return nil, fmt.Errorf("list %s on %s: %w", owner, instance, err)
	}
	return items, nil
}

// Selected is one item the sources select now, and why.
type Selected struct {
	// Provider is the provider instance the item is listed on.
	Provider string
	provider.Item
	// Why are the selectors that took it: "name", "language <language>",
	// "topic <topic>".
	Why []string
}

// Selection is what a workspace's sources select now.
type Selection struct {
	// Items are the selected items, by provider instance and key.
	Items []Selected
	// Excluded, SkippedArchived and SkippedForks count the items a selector
	// matched that the source's exclusions, archived or fork rule left out.
	Excluded        int
	SkippedArchived int
	SkippedForks    int
}

// Resolve lists each source's owner once and selects from it as the sync
// does; an item two sources select is in once, with both reasons.
func (l *Listings) Resolve(ctx context.Context, sources []v1alpha1.Source) (*Selection, error) {
	sel := &Selection{}
	index := map[string]int{}
	for i, s := range sources {
		items, err := l.List(ctx, s.Provider, s.Owner)
		if err != nil {
			return nil, fmt.Errorf("sources[%d]: %w", i, err)
		}
		src := SourceOf(s)
		selected := provider.Select(items, src)
		taken := make(map[string]bool, len(selected))
		for _, it := range selected {
			taken[it.Key()] = true
			key := s.Provider + "/" + it.Key()
			if j, ok := index[key]; ok {
				sel.Items[j].Why = merge(sel.Items[j].Why, why(it, src))
				continue
			}
			index[key] = len(sel.Items)
			sel.Items = append(sel.Items, Selected{Provider: s.Provider, Item: it, Why: why(it, src)})
		}
		// What the selectors match before the exclusions and the archived
		// and fork rules, to say what those left out.
		loose := src
		loose.Exclude, loose.IncludeArchived, loose.IncludeForks = nil, true, true
		excluded := lowerSet(src.Exclude)
		for _, it := range provider.Select(items, loose) {
			switch {
			case taken[it.Key()]:
			case excluded[strings.ToLower(it.Name)]:
				sel.Excluded++
			case it.Archived && !src.IncludeArchived:
				sel.SkippedArchived++
			case it.Fork && !src.IncludeForks:
				sel.SkippedForks++
			}
		}
	}
	sort.Slice(sel.Items, func(i, j int) bool {
		a, b := sel.Items[i], sel.Items[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Key() < b.Key()
	})
	return sel, nil
}

// SourceOf is a Workspace source as the provider selection takes it.
func SourceOf(s v1alpha1.Source) provider.Source {
	src := provider.Source{
		Provider:        s.Provider,
		Owner:           s.Owner,
		Names:           s.Repositories,
		Exclude:         s.Exclude,
		IncludeArchived: s.IncludeArchived,
		IncludeForks:    s.IncludeForks,
	}
	if f := s.Filters; f != nil {
		src.Languages = f.Languages
		if f.Topics != nil {
			src.Topics, src.TopicMatch = f.Topics.Names, provider.TopicMatch(f.Topics.Match)
		}
	}
	return src
}

// why are the selectors of src that take it.
func why(it provider.Item, src provider.Source) []string {
	var out []string
	if lowerSet(src.Names)[strings.ToLower(it.Name)] {
		out = append(out, "name")
	}
	if it.Language != "" && lowerSet(src.Languages)[strings.ToLower(it.Language)] {
		out = append(out, "language "+it.Language)
	}
	topics := lowerSet(src.Topics)
	for _, t := range it.Topics {
		if topics[strings.ToLower(t)] {
			out = append(out, "topic "+t)
		}
	}
	return out
}

func merge(a, b []string) []string {
	for _, s := range b {
		if !slices.Contains(a, s) {
			a = append(a, s)
		}
	}
	return a
}

func lowerSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = true
	}
	return m
}
