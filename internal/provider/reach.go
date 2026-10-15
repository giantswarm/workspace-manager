package provider

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultReachWait is how long a starting manager holds its readiness back
// for a provider that does not answer: past it, the manager is Ready anyway,
// so a provider's outage never keeps the manager out of its Service, and the
// tools say what fails.
const DefaultReachWait = 2 * time.Minute

// reachAttempt bounds one probe of a provider; reachInterval paces them.
const (
	reachAttempt  = 5 * time.Second
	reachInterval = time.Second
)

// ReachURL is where a provider instance is probed: its first API host, or
// its first git host where the API lies under it. Empty without a host.
func ReachURL(h Hosts) string {
	for _, hs := range [][]Host{h.API, h.Git} {
		if len(hs) > 0 {
			return "https://" + hs[0].Name + hs[0].PathPrefix
		}
	}
	return ""
}

// Reach is a starting manager's readiness on the provider path: Ready once
// every provider instance answered one request, with any status, over the
// HTTP client its API calls use, or once the wait is over. It never turns
// unready again: a later outage is the tools' answer to give, not an empty
// Service's.
type Reach struct {
	Instances []Instance
	// HTTP is the client the kinds' API calls use; nil is
	// http.DefaultClient.
	HTTP *http.Client
	// Wait is how long readiness is held back; zero is DefaultReachWait.
	Wait time.Duration
	// Interval paces the probes of a provider that has not answered; zero is
	// one second.
	Interval time.Duration
	Logger   *slog.Logger

	ready atomic.Bool
}

// Ready reports whether every provider answered, or the wait is over.
func (r *Reach) Ready() bool { return r.ready.Load() }

// Run probes every provider until each answered once, the wait is over or
// ctx ends; then the manager is Ready.
func (r *Reach) Run(ctx context.Context) {
	defer r.ready.Store(true)
	log := r.Logger
	if log == nil {
		log = slog.Default()
	}
	wait := r.Wait
	if wait == 0 {
		wait = DefaultReachWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var wg sync.WaitGroup
	for _, in := range r.Instances {
		u := ReachURL(in.Hosts())
		if u == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			if err := r.probe(ctx, u); err != nil {
				log.Warn("ready without an answer from the provider", "provider", in.Name, "url", u, "waited", wait, "error", err)
				return
			}
			log.Info("provider answers", "provider", in.Name, "url", u, "after", time.Since(start).Round(time.Millisecond))
		}()
	}
	wg.Wait()
}

// probe asks u until it answers or ctx ends, returning the last error.
func (r *Reach) probe(ctx context.Context, u string) error {
	hc := r.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	interval := r.Interval
	if interval == 0 {
		interval = reachInterval
	}
	for {
		err := reachOnce(ctx, hc, u)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(interval):
		}
	}
}

func reachOnce(ctx context.Context, hc *http.Client, u string) error {
	ctx, cancel := context.WithTimeout(ctx, reachAttempt)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
