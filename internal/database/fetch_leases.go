package database

import (
	"fmt"
	"time"
)

// Fetch leases let instances sharing a Postgres database fetch each cache
// miss from upstream once. The instance that fetches holds the lease for the
// miss's key and renews it until the result is stored; the others wait for
// the stored result. Expiry uses the database clock, so instances whose
// clocks disagree still agree on when a lease has run out. SQLite is
// single-instance, so these are only used on Postgres.

// AcquireFetchLease takes the lease on key for owner unless another owner
// holds one that has not expired, and reports whether it did.
func (db *DB) AcquireFetchLease(key, owner string, ttl time.Duration) (bool, error) {
	res, err := db.Exec(`
		INSERT INTO fetch_leases (key, owner, expires_at)
		VALUES ($1, $2, now() + $3 * interval '1 millisecond')
		ON CONFLICT (key) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at
		WHERE fetch_leases.expires_at < now()`, key, owner, ttl.Milliseconds())
	if err != nil {
		return false, fmt.Errorf("acquiring fetch lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("acquiring fetch lease: %w", err)
	}
	return n > 0, nil
}

// RenewFetchLease extends owner's lease on key by ttl from now, and reports
// whether owner still held it.
func (db *DB) RenewFetchLease(key, owner string, ttl time.Duration) (bool, error) {
	res, err := db.Exec(`
		UPDATE fetch_leases SET expires_at = now() + $3 * interval '1 millisecond'
		WHERE key = $1 AND owner = $2`, key, owner, ttl.Milliseconds())
	if err != nil {
		return false, fmt.Errorf("renewing fetch lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("renewing fetch lease: %w", err)
	}
	return n > 0, nil
}

// ReleaseFetchLease drops owner's lease on key.
func (db *DB) ReleaseFetchLease(key, owner string) error {
	if _, err := db.Exec(`DELETE FROM fetch_leases WHERE key = $1 AND owner = $2`, key, owner); err != nil {
		return fmt.Errorf("releasing fetch lease: %w", err)
	}
	return nil
}

// DeleteExpiredFetchLeases removes leases that expired more than grace ago,
// left by instances that stopped while fetching.
func (db *DB) DeleteExpiredFetchLeases(grace time.Duration) (int64, error) {
	if db.dialect != DialectPostgres {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM fetch_leases WHERE expires_at < now() - $1 * interval '1 millisecond'`, grace.Milliseconds())
	if err != nil {
		return 0, fmt.Errorf("deleting expired fetch leases: %w", err)
	}
	return res.RowsAffected()
}
