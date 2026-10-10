package provider

import (
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ListingFreshness is how long a kind answers List from a listing it made:
// the keystrokes of an editor's picker and the workspaces resolved on one
// owner together share one listing, and a change at the provider shows within
// it.
const ListingFreshness = time.Minute

// ListingCache holds one listing per owner for ListingFreshness. Callers on an
// owner share a listing in flight, so an owner of hundreds of items costs one
// paginated listing however many ask at once. A failed listing is not kept.
type ListingCache struct {
	clock Clock

	mu      sync.Mutex
	entries map[string]listing
	flight  singleflight.Group
}

type listing struct {
	items []Item
	at    time.Time
}

// NewListingCache returns an empty cache going by clock.
func NewListingCache(clock Clock) *ListingCache {
	return &ListingCache{clock: clock, entries: map[string]listing{}}
}

// Get returns owner's items, from the cache while its listing is fresh, else
// from list. The result is the caller's own slice.
func (c *ListingCache) Get(owner string, list func() ([]Item, error)) ([]Item, error) {
	if items, ok := c.fresh(owner); ok {
		return items, nil
	}
	v, err, _ := c.flight.Do(owner, func() (any, error) {
		items, err := list()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.entries[owner] = listing{items: items, at: c.clock.Now()}
		c.mu.Unlock()
		return items, nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Clone(v.([]Item)), nil
}

func (c *ListingCache) fresh(owner string) ([]Item, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.entries[owner]
	if !ok || c.clock.Now().Sub(l.at) >= ListingFreshness {
		return nil, false
	}
	return slices.Clone(l.items), true
}
