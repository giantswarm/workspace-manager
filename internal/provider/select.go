package provider

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// TopicMatch is how a Source's topics select.
type TopicMatch string

const (
	// TopicsAny selects an item carrying at least one of the topics.
	TopicsAny TopicMatch = "any"
	// TopicsAll selects an item carrying every one of the topics.
	TopicsAll TopicMatch = "all"
)

// Source is one owner on one provider instance and what to take from it.
type Source struct {
	// Provider is the provider instance's name.
	Provider string
	// Owner is the owner on that provider.
	Owner string
	// Names are items selected by name, whatever the filters say.
	Names []string
	// Languages select items whose primary language is any of them.
	Languages []string
	// Topics select items by topic, as TopicMatch says (TopicsAny when
	// empty).
	Topics     []string
	TopicMatch TopicMatch
	// Exclude drops items by name, named ones included.
	Exclude []string
	// IncludeArchived and IncludeForks let archived items and forks match the
	// filters; a named item counts either way.
	IncludeArchived bool
	IncludeForks    bool
}

// Select resolves a Source over what its provider listed for the owner, in
// name order. An item is selected when it is named, or when it passes every
// filter that is set (Languages, Topics) and is neither archived nor a fork
// unless those are included; a Source with neither names nor filters takes
// every item on those terms. Exclude wins over everything. Names, languages
// and topics compare without case.
func Select(items []Item, src Source) []Item {
	named := lowerSet(src.Names)
	excluded := lowerSet(src.Exclude)
	langs := lowerSet(src.Languages)
	topics := lowerSet(src.Topics)
	filtered := len(langs) > 0 || len(topics) > 0
	takeAll := !filtered && len(named) == 0

	var out []Item
	for _, it := range items {
		name := strings.ToLower(it.Name)
		if excluded[name] {
			continue
		}
		switch {
		case named[name]:
		case !takeAll && !filtered:
			continue
		case it.Archived && !src.IncludeArchived, it.Fork && !src.IncludeForks:
			continue
		case len(langs) > 0 && !langs[strings.ToLower(it.Language)]:
			continue
		case len(topics) > 0 && !topicsMatch(it.Topics, topics, src.TopicMatch):
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

func topicsMatch(have []string, want map[string]bool, match TopicMatch) bool {
	hits := 0
	for t := range lowerSet(have) {
		if want[t] {
			hits++
		}
	}
	if match == TopicsAll {
		return hits == len(want)
	}
	return hits > 0
}

func lowerSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[strings.ToLower(s)] = true
	}
	return m
}

// State is a recorded selection: each item's key and its last change. A sync
// records it in its manifest and compares the next listing with it.
type State map[string]time.Time

// StateOf records items.
func StateOf(items []Item) State {
	s := make(State, len(items))
	for _, it := range items {
		s[it.Key()] = it.LastChange
	}
	return s
}

// Changes is what differs between a recorded State and a new selection, each
// list of item keys sorted.
type Changes struct {
	// Added items are new in the selection.
	Added []string
	// Removed items left the selection.
	Removed []string
	// Changed items stayed and changed after their recorded last change.
	Changed []string
}

// None is true when nothing changed: the sync ends without a fetch.
func (c Changes) None() bool {
	return len(c.Added) == 0 && len(c.Removed) == 0 && len(c.Changed) == 0
}

// Diff compares a new selection with the recorded State.
func Diff(recorded State, items []Item) Changes {
	var c Changes
	seen := make(map[string]bool, len(items))
	for _, it := range items {
		k := it.Key()
		seen[k] = true
		last, ok := recorded[k]
		switch {
		case !ok:
			c.Added = append(c.Added, k)
		case it.LastChange.After(last):
			c.Changed = append(c.Changed, k)
		}
	}
	for k := range recorded {
		if !seen[k] {
			c.Removed = append(c.Removed, k)
		}
	}
	slices.Sort(c.Added)
	slices.Sort(c.Removed)
	slices.Sort(c.Changed)
	return c
}
