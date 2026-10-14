package handler

import (
	"time"

	"github.com/git-pkgs/proxy/internal/database"
)

// metadataDateAttempts bounds how often metadataDate retries when other
// replicas keep changing a package's date between its read and its write.
const metadataDateAttempts = 3

// metadataDate returns the Last-Modified to serve a package's metadata with,
// as rewritten for proxyURL into the representation with the given ETag, or
// zero for none.
//
// The date is the second the package started serving that representation,
// recorded in the database so that every replica sharing it serves the same
// date. A switch to another representation is dated only in a second later
// than the recorded date, and then with that second, so a date is never given
// to two representations of a package: a client holding one never gets 304
// for another, whichever replica it asks. Until such a second the
// representation goes undated, and If-Modified-Since is not evaluated.
//
// A package's first date is likewise later than the second this process
// started, as the process it replaced may have dated the package then without
// recording it. A date ahead of this process's clock, recorded by a replica
// whose clock is ahead, is not served until the clock catches up. Without a
// database, or when it fails, nothing is dated.
func (p *Proxy) metadataDate(ecosystem, proxyURL, name, etag string) time.Time {
	if p.DB == nil || etag == "" {
		return time.Time{}
	}
	now := p.clock().Truncate(time.Second)
	for range metadataDateAttempts {
		date, settled, err := p.claimMetadataDate(ecosystem, proxyURL, name, etag, now)
		if err != nil {
			p.Logger.Warn("failed to date rewritten metadata", "ecosystem", ecosystem, "name", name, "error", err)
			return time.Time{}
		}
		if settled {
			if date.After(now) {
				return time.Time{}
			}
			return date
		}
	}
	return time.Time{}
}

// claimMetadataDate makes one attempt at recording the date of etag for a
// package at now. It reports false when another replica changed the date
// first, and the attempt should be made again.
func (p *Proxy) claimMetadataDate(ecosystem, proxyURL, name, etag string, now time.Time) (time.Time, bool, error) {
	current, err := p.DB.GetMetadataDate(ecosystem, proxyURL, name)
	if err != nil {
		return time.Time{}, false, err
	}
	next := database.MetadataDate{ETag: etag, LastModified: now}
	switch {
	case current == nil:
		if !now.After(p.started.Truncate(time.Second)) {
			return time.Time{}, true, nil
		}
		inserted, err := p.DB.InsertMetadataDate(ecosystem, proxyURL, name, next)
		return now, inserted, err
	case current.ETag == etag:
		return current.LastModified, true, nil
	case !now.After(current.LastModified):
		return time.Time{}, true, nil
	default:
		swapped, err := p.DB.SwapMetadataDate(ecosystem, proxyURL, name, *current, next)
		return now, swapped, err
	}
}

// clock reads the time metadata is dated by.
func (p *Proxy) clock() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}
