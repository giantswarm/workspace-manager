package provider

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stoppedClock moves only when told.
type stoppedClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stoppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stoppedClock) Sleep(context.Context, time.Duration) error { return nil }

func (c *stoppedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestListingCacheServesAFreshListing(t *testing.T) {
	clock := &stoppedClock{now: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	c := NewListingCache(clock)
	var listings atomic.Int32
	list := func() ([]Item, error) {
		listings.Add(1)
		return []Item{{Owner: "acme", Name: "api", Topics: []string{"platform"}}}, nil
	}

	first, err := c.Get("acme", list)
	require.NoError(t, err)
	clock.advance(ListingFreshness - time.Second)
	second, err := c.Get("acme", list)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.EqualValues(t, 1, listings.Load(), "one listing within the freshness")

	second[0].Name = "changed"
	third, err := c.Get("acme", list)
	require.NoError(t, err)
	assert.Equal(t, "api", third[0].Name, "a caller's slice is its own")

	clock.advance(time.Second)
	_, err = c.Get("acme", list)
	require.NoError(t, err)
	assert.EqualValues(t, 2, listings.Load(), "a new listing once the freshness passed")
}

func TestListingCacheIsPerOwner(t *testing.T) {
	c := NewListingCache(&stoppedClock{now: time.Now()})
	owners := map[string]int{}
	for _, owner := range []string{"acme", "umbrella", "acme"} {
		_, err := c.Get(owner, func() ([]Item, error) {
			owners[owner]++
			return nil, nil
		})
		require.NoError(t, err)
	}
	assert.Equal(t, map[string]int{"acme": 1, "umbrella": 1}, owners)
}

func TestListingCacheKeepsNoFailedListing(t *testing.T) {
	c := NewListingCache(&stoppedClock{now: time.Now()})
	calls := 0
	list := func() ([]Item, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("rate limited")
		}
		return []Item{{Owner: "acme", Name: "api"}}, nil
	}
	_, err := c.Get("acme", list)
	require.EqualError(t, err, "rate limited")
	items, err := c.Get("acme", list)
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, 2, calls)
}

func TestListingCacheSharesAListingInFlight(t *testing.T) {
	c := NewListingCache(&stoppedClock{now: time.Now()})
	var listings atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	list := func() ([]Item, error) {
		listings.Add(1)
		close(started)
		<-release
		return []Item{{Owner: "acme", Name: "api"}}, nil
	}

	var wg sync.WaitGroup
	results := make([][]Item, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := c.Get("acme", list)
			assert.NoError(t, err)
			results[i] = items
		}()
		if i == 0 {
			<-started
		}
	}
	close(release)
	wg.Wait()
	assert.EqualValues(t, 1, listings.Load(), "the second caller waited for the first's listing")
	assert.Equal(t, results[0], results[1])
}
