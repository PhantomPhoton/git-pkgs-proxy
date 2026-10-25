package database

import (
	"fmt"
	"time"
)

// Upstream holds let instances sharing a database honour a rate-limit hold
// any one of them placed, so each does not need its own 429 before it stops
// sending. Times are the instances' own clocks in Unix milliseconds; a few
// seconds of skew does not matter next to holds of a minute or more.

type upstreamHoldRow struct {
	Host    string `db:"host"`
	UntilMS int64  `db:"until_ms"`
}

// PutUpstreamHold records that requests to host are held until until. A hold
// already recorded for longer is kept.
func (db *DB) PutUpstreamHold(host string, until time.Time) error {
	_, err := db.Exec(db.Rebind(`
		INSERT INTO upstream_holds (host, until_ms) VALUES (?, ?)
		ON CONFLICT (host) DO UPDATE SET until_ms = CASE
			WHEN excluded.until_ms > upstream_holds.until_ms THEN excluded.until_ms
			ELSE upstream_holds.until_ms END
	`), host, until.UnixMilli())
	if err != nil {
		return fmt.Errorf("recording upstream hold: %w", err)
	}
	return nil
}

// UpstreamHolds returns the holds that have not ended by now, and deletes
// those that have.
func (db *DB) UpstreamHolds(now time.Time) (map[string]time.Time, error) {
	var rows []upstreamHoldRow
	if err := db.Select(&rows, `SELECT host, until_ms FROM upstream_holds`); err != nil {
		return nil, fmt.Errorf("reading upstream holds: %w", err)
	}
	holds := make(map[string]time.Time, len(rows))
	expired := false
	for _, row := range rows {
		until := time.UnixMilli(row.UntilMS)
		if now.Before(until) {
			holds[row.Host] = until
		} else {
			expired = true
		}
	}
	// Only write when there is something to delete, so the frequent reads
	// stay reads.
	if expired {
		if _, err := db.Exec(db.Rebind(`DELETE FROM upstream_holds WHERE until_ms <= ?`), now.UnixMilli()); err != nil {
			return holds, fmt.Errorf("deleting expired upstream holds: %w", err)
		}
	}
	return holds, nil
}
