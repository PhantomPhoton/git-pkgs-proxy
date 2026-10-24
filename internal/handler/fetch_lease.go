package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/git-pkgs/proxy/internal/database"
	"github.com/git-pkgs/proxy/internal/metrics"
)

// Instances sharing a Postgres database coalesce cache misses through fetch
// leases, as coalesceFetch and coalesceMetadata do within one instance. The
// instance that runs the fetch holds the lease for the miss and renews it
// while it downloads. Another instance missing the same key waits for the
// stored result and serves that, or takes the lease over if the holder
// releases it without storing anything or stops renewing it.
var (
	fetchLeaseTTL   = 30 * time.Second
	fetchLeaseRenew = 10 * time.Second
	// The wait for another instance's fetch polls the cache, starting
	// quickly for small files and backing off for large ones.
	fetchLeasePollMin = 50 * time.Millisecond
	fetchLeasePollMax = 2 * time.Second
)

// fetchLeaseKey turns a coalescing key, which can hold long URLs and NUL
// separators, into a fixed-size table key.
func fetchLeaseKey(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

func newLeaseOwner() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// fetchLeasesEnabled reports whether misses are coalesced across instances.
func (p *Proxy) fetchLeasesEnabled() bool {
	return p.DB != nil && p.DB.Dialect() == database.DialectPostgres
}

// leaseOutcome says how a caller goes on after awaitFetchLease.
type leaseOutcome int

const (
	// leaseFetch: this instance fetches, and calls release afterwards.
	leaseFetch leaseOutcome = iota
	// leaseStored: ready found the result another instance stored.
	leaseStored
	// leaseFailed: the fetch this caller waited on failed. It fetches
	// without the lease, as every instance that waited does at the same
	// time, or serves a stale copy, rather than queueing behind the others.
	leaseFailed
)

// awaitFetchLease takes the lease on key before this instance fetches it from
// upstream. While another instance holds the lease, it polls ready, which
// reports whether that instance's result is now stored and, if so, keeps it
// for the caller. release is to be called with whether the fetch failed once
// it is done; err is set when ctx ended while waiting.
//
// A failing lease query is logged and the fetch goes ahead without a lease,
// so a database hiccup costs a duplicate fetch rather than the request.
func (p *Proxy) awaitFetchLease(ctx context.Context, kind, key string, ready func() bool) (release func(failed bool), outcome leaseOutcome, err error) {
	noop := func(bool) {}
	if !p.fetchLeasesEnabled() {
		return noop, leaseFetch, nil
	}
	leaseKey := fetchLeaseKey(kind, key)
	owner := newLeaseOwner()
	delay := fetchLeasePollMin
	waited := false
	for {
		acquired, err := p.DB.AcquireFetchLease(leaseKey, owner, fetchLeaseTTL)
		if err != nil {
			p.Logger.Warn("fetch lease unavailable, fetching without it", "kind", kind, "error", err)
			return noop, leaseFetch, nil
		}
		if acquired {
			// The holder may have stored the result and released the lease
			// between this caller's last poll and taking it.
			if waited && ready() {
				p.releaseFetchLease(leaseKey, owner)
				return noop, leaseStored, nil
			}
			return p.holdFetchLease(leaseKey, owner), leaseFetch, nil
		}
		if !waited {
			metrics.RecordFetchLeaseWait(kind)
			waited = true
		}
		outcome, done, err := p.waitForLeaseHolder(ctx, kind, leaseKey, ready, &delay)
		if done || err != nil {
			return noop, outcome, err
		}
	}
}

// waitForLeaseHolder polls ready while another instance holds the lease on
// leaseKey. It is done when ready found the result, when the fetch it waited
// on failed, or when the lease can't be read; otherwise it returns once the
// lease is free to take.
func (p *Proxy) waitForLeaseHolder(ctx context.Context, kind, leaseKey string, ready func() bool, delay *time.Duration) (outcome leaseOutcome, done bool, err error) {
	holder := ""
	for {
		current, failed, held, err := p.DB.FetchLeaseHolder(leaseKey)
		if err != nil {
			p.Logger.Warn("fetch lease unavailable, fetching without it", "kind", kind, "error", err)
			return leaseFetch, true, nil
		}
		if failed && current == holder {
			return leaseFailed, true, nil
		}
		if !held || failed {
			// Released, expired, or failed before this caller waited on
			// it: free to take.
			return leaseFetch, false, nil
		}
		holder = current
		timer := time.NewTimer(*delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return leaseFetch, true, ctx.Err()
		case <-timer.C:
		}
		if ready() {
			return leaseStored, true, nil
		}
		*delay = min(*delay+*delay, fetchLeasePollMax)
	}
}

// holdFetchLease renews owner's lease on leaseKey until the returned release
// is called. release drops the lease, or marks it failed so instances
// waiting on it stop waiting.
func (p *Proxy) holdFetchLease(leaseKey, owner string) func(failed bool) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(fetchLeaseRenew)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				// A lease that lapsed, say after a long pause, lets another
				// instance fetch too. That costs a duplicate fetch, nothing
				// else, so the fetch here carries on either way.
				if held, err := p.DB.RenewFetchLease(leaseKey, owner, fetchLeaseTTL); err != nil || !held {
					p.Logger.Debug("fetch lease lapsed", "error", err)
				}
			}
		}
	}()
	return func(failed bool) {
		close(stop)
		<-done
		if !failed {
			p.releaseFetchLease(leaseKey, owner)
			return
		}
		if err := p.DB.FailFetchLease(leaseKey, owner); err != nil {
			// Waiting instances take the lease once it expires.
			p.Logger.Warn("failed to mark fetch lease failed", "error", err)
		}
	}
}

func (p *Proxy) releaseFetchLease(leaseKey, owner string) {
	if err := p.DB.ReleaseFetchLease(leaseKey, owner); err != nil {
		// The lease expires on its own; until then other instances wait
		// on it and then fetch.
		p.Logger.Warn("failed to release fetch lease", "error", err)
	}
}
