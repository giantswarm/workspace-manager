package provider

import (
	"context"
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

// freshListing marks a context whose List makes a listing of its own.
type freshListing struct{}

// FreshListing is ctx for a List that never answers from the cache: a
// requested sync reads what the provider holds now, however recently the
// owner was listed. Its listing refreshes the cache for the callers after it.
func FreshListing(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshListing{}, true)
}

func isFreshListing(ctx context.Context) bool {
	fresh, _ := ctx.Value(freshListing{}).(bool)
	return fresh
}

// Get returns owner's items, from the cache while its listing is fresh, else
// from list; under FreshListing always from list. The result is the caller's
// own slice.
func (c *ListingCache) Get(ctx context.Context, owner string, list func() ([]Item, error)) ([]Item, error) {
	key := owner
	if isFreshListing(ctx) {
		// A listing in flight may have started before this call: it shares
		// only one started under FreshListing too.
		key = "fresh\x00" + owner
	} else if items, ok := c.fresh(owner); ok {
		return items, nil
	}
	v, err, _ := c.flight.Do(key, func() (any, error) {
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
