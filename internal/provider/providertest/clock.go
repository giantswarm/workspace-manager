package providertest

import (
	"context"
	"sync"
	"time"
)

// Clock is a stopped clock for a kind's stub provider: it moves when
// advanced, and a Sleep moves it by the duration slept, which it records.
type Clock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

// NewClock returns a clock stopped at now.
func NewClock(now time.Time) *Clock { return &Clock{now: now} }

// Now implements provider.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep implements provider.Clock without waiting.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	return nil
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Slept are the waits so far.
func (c *Clock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}
