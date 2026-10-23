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

// awaitFetchLease takes the lease on key before this instance fetches it from
// upstream. While another instance holds the lease, it polls ready, which
// reports whether that instance's result is now stored and, if so, keeps it
// for the caller. It returns:
//   - stored true when ready found the result: serve it, nothing to release.
//   - release when this instance should fetch: call it once the result is
//     stored or the fetch failed.
//   - err when ctx ended while waiting.
//
// A failing lease query is logged and the fetch goes ahead without a lease,
// so a database hiccup costs a duplicate fetch rather than the request.
func (p *Proxy) awaitFetchLease(ctx context.Context, kind, key string, ready func() bool) (release func(), stored bool, err error) {
	noop := func() {}
	if !p.fetchLeasesEnabled() {
		return noop, false, nil
	}
	leaseKey := fetchLeaseKey(kind, key)
	owner := newLeaseOwner()
	delay := fetchLeasePollMin
	waited := false
	for {
		acquired, err := p.DB.AcquireFetchLease(leaseKey, owner, fetchLeaseTTL)
		if err != nil {
			p.Logger.Warn("fetch lease unavailable, fetching without it", "kind", kind, "error", err)
			return noop, false, nil
		}
		if acquired {
			// The holder may have stored the result and released the lease
			// between this caller's last poll and taking it.
			if waited && ready() {
				p.releaseFetchLease(leaseKey, owner)
				return noop, true, nil
			}
			return p.holdFetchLease(leaseKey, owner), false, nil
		}
		if !waited {
			metrics.RecordFetchLeaseWait(kind)
			waited = true
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false, ctx.Err()
		case <-timer.C:
		}
		if ready() {
			return noop, true, nil
		}
		delay = min(delay+delay, fetchLeasePollMax)
	}
}

// holdFetchLease renews owner's lease on leaseKey until the returned release
// is called, which also drops it.
func (p *Proxy) holdFetchLease(leaseKey, owner string) func() {
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
	return func() {
		close(stop)
		<-done
		p.releaseFetchLease(leaseKey, owner)
	}
}

func (p *Proxy) releaseFetchLease(leaseKey, owner string) {
	if err := p.DB.ReleaseFetchLease(leaseKey, owner); err != nil {
		// The lease expires on its own; until then other instances wait
		// on it and then fetch.
		p.Logger.Warn("failed to release fetch lease", "error", err)
	}
}
