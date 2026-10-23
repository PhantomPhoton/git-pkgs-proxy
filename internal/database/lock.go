package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"hash/fnv"
	"time"
)

// Several instances can share one Postgres database. Advisory locks keep
// them from creating the schema, migrating it or running a background job at
// the same time. SQLite is single-instance, so there every lock is granted
// at once.

// lockKey maps a lock name to the 64-bit key Postgres advisory locks take.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("git-pkgs/proxy:" + name))
	return int64(h.Sum64()) //nolint:gosec // a key, wrapping is fine
}

// WithLock runs fn while holding the named lock, waiting for another
// instance that holds it.
func (db *DB) WithLock(ctx context.Context, name string, fn func() error) error {
	if db.dialect != DialectPostgres {
		return fn()
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("taking connection for lock %s: %w", name, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", lockKey(name)); err != nil {
		return fmt.Errorf("taking lock %s: %w", name, err)
	}
	defer unlock(conn, name)
	return fn()
}

// TryLock takes the named lock if no other instance holds it. When ok is
// true the caller must call release once done. The lock lives in a
// connection held for that time, and Postgres frees it if the process dies.
func (db *DB) TryLock(ctx context.Context, name string) (release func(), ok bool, err error) {
	if db.dialect != DialectPostgres {
		return func() {}, true, nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("taking connection for lock %s: %w", name, err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", lockKey(name)).Scan(&ok); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("taking lock %s: %w", name, err)
	}
	if !ok {
		_ = conn.Close()
		return nil, false, nil
	}
	return func() {
		unlock(conn, name)
		_ = conn.Close()
	}, true, nil
}

// unlockTimeout bounds the unlock. If it fails, closing the connection still
// ends the session that holds the lock.
const unlockTimeout = 10 * time.Second

func unlock(conn *sql.Conn, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", lockKey(name)); err != nil {
		// Drop the connection so the session, and with it the lock, ends.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
}

// ClaimJobRun records that the named job starts now unless it started less
// than minGap ago, and reports whether the caller should run it. Instances
// sharing the database each keep their own timer for a scheduled job, and
// this lets only the first of them run it per interval.
func (db *DB) ClaimJobRun(name string, minGap time.Duration) (bool, error) {
	now := time.Now().UTC()
	res, err := db.Exec(db.Rebind(`
		INSERT INTO job_runs (name, started_at) VALUES (?, ?)
		ON CONFLICT(name) DO UPDATE SET started_at = excluded.started_at
		WHERE job_runs.started_at <= ?`), name, now, now.Add(-minGap))
	if err != nil {
		return false, fmt.Errorf("claiming job run %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claiming job run %s: %w", name, err)
	}
	return n > 0, nil
}
