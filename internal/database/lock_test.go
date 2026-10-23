package database

import (
	"context"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTryLockSQLiteAlwaysGranted(t *testing.T) {
	db := createTestDB(t)
	defer func() { _ = db.Close() }()
	release, ok, err := db.TryLock(context.Background(), "job:test")
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v; want granted", ok, err)
	}
	// SQLite is single-instance, so a second holder is granted too.
	release2, ok, err := db.TryLock(context.Background(), "job:test")
	if err != nil || !ok {
		t.Fatalf("second TryLock = %v, %v; want granted", ok, err)
	}
	release2()
	release()
}

func TestClaimJobRun(t *testing.T) {
	forEachDialect(t, func(t *testing.T, db *DB) {
		claimed, err := db.ClaimJobRun("mirror-schedule:a", time.Hour)
		if err != nil || !claimed {
			t.Fatalf("first claim = %v, %v; want claimed", claimed, err)
		}
		claimed, err = db.ClaimJobRun("mirror-schedule:a", time.Hour)
		if err != nil || claimed {
			t.Fatalf("claim within the gap = %v, %v; want not claimed", claimed, err)
		}
		claimed, err = db.ClaimJobRun("mirror-schedule:b", time.Hour)
		if err != nil || !claimed {
			t.Fatalf("claim of another job = %v, %v; want claimed", claimed, err)
		}
		// A run started longer ago than the gap can be claimed again.
		claimed, err = db.ClaimJobRun("mirror-schedule:a", 0)
		if err != nil || !claimed {
			t.Fatalf("claim after the gap = %v, %v; want claimed", claimed, err)
		}
	})
}

// forEachDialect runs fn against SQLite and, when PROXY_DATABASE_URL is set,
// against Postgres.
func forEachDialect(t *testing.T, fn func(t *testing.T, db *DB)) {
	t.Run("sqlite", func(t *testing.T) {
		db := createTestDB(t)
		defer func() { _ = db.Close() }()
		fn(t, db)
	})
	t.Run("postgres", func(t *testing.T) {
		db := createTestPostgresDB(t)
		if db == nil {
			t.Skip("PROXY_DATABASE_URL not set")
		}
		defer func() { _ = db.Close() }()
		fn(t, db)
	})
}

func openTestPostgres(t *testing.T) *DB {
	t.Helper()
	dbURL := os.Getenv("PROXY_DATABASE_URL")
	if dbURL == "" {
		t.Skip("PROXY_DATABASE_URL not set")
	}
	db, err := OpenPostgres(dbURL)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestTryLockPostgresExcludesOtherInstance(t *testing.T) {
	a := openTestPostgres(t)
	b := openTestPostgres(t)
	ctx := context.Background()

	release, ok, err := a.TryLock(ctx, "job:test-exclusive")
	if err != nil || !ok {
		t.Fatalf("TryLock on a = %v, %v; want granted", ok, err)
	}
	if _, ok, err := b.TryLock(ctx, "job:test-exclusive"); err != nil || ok {
		t.Fatalf("TryLock on b while a holds it = %v, %v; want refused", ok, err)
	}
	// Another lock name is independent.
	releaseOther, ok, err := b.TryLock(ctx, "job:test-other")
	if err != nil || !ok {
		t.Fatalf("TryLock on another name = %v, %v; want granted", ok, err)
	}
	releaseOther()

	release()
	release, ok, err = b.TryLock(ctx, "job:test-exclusive")
	if err != nil || !ok {
		t.Fatalf("TryLock on b after a released = %v, %v; want granted", ok, err)
	}
	release()
}

func TestTryLockPostgresFreedWhenSessionEnds(t *testing.T) {
	a := openTestPostgres(t)
	b := openTestPostgres(t)
	ctx := context.Background()

	if _, ok, err := a.TryLock(ctx, "job:test-crash"); err != nil || !ok {
		t.Fatalf("TryLock on a = %v, %v; want granted", ok, err)
	}
	// End a's session holding the lock, as a crash would.
	if _, err := b.Exec(`SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND pid <> pg_backend_pid()`); err != nil {
		t.Fatalf("terminating a's session: %v", err)
	}
	waitUntil(t, func() bool {
		release, ok, err := b.TryLock(ctx, "job:test-crash")
		if ok {
			release()
		}
		return err == nil && ok
	})
}

func TestWithLockPostgresSerialises(t *testing.T) {
	const instances = 4
	dbs := make([]*DB, instances)
	for i := range dbs {
		dbs[i] = openTestPostgres(t)
	}
	var inside, overlapped atomic.Int32
	var wg sync.WaitGroup
	for _, db := range dbs {
		wg.Go(func() {
			err := db.WithLock(context.Background(), "test-serialise", func() error {
				if inside.Add(1) > 1 {
					overlapped.Store(1)
				}
				time.Sleep(20 * time.Millisecond)
				inside.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("WithLock: %v", err)
			}
		})
	}
	wg.Wait()
	if overlapped.Load() != 0 {
		t.Error("two instances held the lock at once")
	}
}

// TestOpenPostgresOrCreateConcurrent starts several instances at once on an
// empty database. Without the schema lock all but one failed on each
// other's tables.
func TestOpenPostgresOrCreateConcurrent(t *testing.T) {
	admin := openTestPostgres(t)
	name := "proxy_lock_test_" + strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "_")
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Skipf("cannot create a scratch database: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })

	u, err := url.Parse(os.Getenv("PROXY_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parsing PROXY_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	scratch := u.String()

	const instances = 5
	errs := make([]error, instances)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range instances {
		wg.Go(func() {
			<-start
			db, err := OpenPostgresOrCreate(scratch)
			if err == nil {
				err = db.MigrateSchema()
				_ = db.Close()
			}
			errs[i] = err
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("instance %d: %v", i, err)
		}
	}

	db, err := OpenPostgres(scratch)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.Get(&n, "SELECT COUNT(*) FROM schema_info"); err != nil || n != 1 {
		t.Errorf("schema_info rows = %d, %v; want 1", n, err)
	}
	if err := db.Get(&n, "SELECT COUNT(*) FROM migrations"); err != nil || n != len(migrations) {
		t.Errorf("migrations rows = %d, %v; want %d", n, err, len(migrations))
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMigrateAddsJobRuns(t *testing.T) {
	forEachDialect(t, func(t *testing.T, db *DB) {
		// A database from before the job_runs migration.
		if _, err := db.Exec("DROP TABLE job_runs"); err != nil {
			t.Fatalf("dropping job_runs: %v", err)
		}
		if _, err := db.Exec(db.Rebind("DELETE FROM migrations WHERE name = ?"), "015_add_job_runs"); err != nil {
			t.Fatalf("forgetting the migration: %v", err)
		}
		if err := db.MigrateSchema(); err != nil {
			t.Fatalf("MigrateSchema: %v", err)
		}
		if claimed, err := db.ClaimJobRun("mirror-schedule:a", time.Hour); err != nil || !claimed {
			t.Fatalf("ClaimJobRun after migration = %v, %v; want claimed", claimed, err)
		}
	})
}

func TestFetchLeases(t *testing.T) {
	db := createTestPostgresDB(t)
	if db == nil {
		t.Skip("PROXY_DATABASE_URL not set")
	}
	defer func() { _ = db.Close() }()

	acquire := func(owner string, ttl time.Duration) bool {
		t.Helper()
		ok, err := db.AcquireFetchLease("k", owner, ttl)
		if err != nil {
			t.Fatalf("AcquireFetchLease(%s): %v", owner, err)
		}
		return ok
	}
	if !acquire("a", time.Minute) {
		t.Fatal("a could not take a free lease")
	}
	if acquire("b", time.Minute) {
		t.Fatal("b took a lease a holds")
	}
	if held, err := db.RenewFetchLease("k", "b", time.Minute); err != nil || held {
		t.Fatalf("b renewed a's lease: %v, %v", held, err)
	}
	if held, err := db.RenewFetchLease("k", "a", time.Minute); err != nil || !held {
		t.Fatalf("a could not renew its lease: %v, %v", held, err)
	}
	// Only the owner can release it.
	if err := db.ReleaseFetchLease("k", "b"); err != nil {
		t.Fatal(err)
	}
	if acquire("b", time.Minute) {
		t.Fatal("b's release dropped a's lease")
	}
	if err := db.ReleaseFetchLease("k", "a"); err != nil {
		t.Fatal(err)
	}
	if !acquire("b", time.Millisecond) {
		t.Fatal("b could not take the lease a released")
	}
	time.Sleep(20 * time.Millisecond)
	if !acquire("c", time.Millisecond) {
		t.Fatal("c could not take b's expired lease")
	}
	time.Sleep(20 * time.Millisecond)
	if n, err := db.DeleteExpiredFetchLeases(0); err != nil || n != 1 {
		t.Fatalf("DeleteExpiredFetchLeases = %d, %v; want 1", n, err)
	}
}

func TestFetchLeaseFailure(t *testing.T) {
	db := createTestPostgresDB(t)
	if db == nil {
		t.Skip("PROXY_DATABASE_URL not set")
	}
	defer func() { _ = db.Close() }()

	if ok, err := db.AcquireFetchLease("k", "a", time.Minute); err != nil || !ok {
		t.Fatalf("a could not take a free lease: %v, %v", ok, err)
	}
	if owner, failed, ok, err := db.FetchLeaseHolder("k"); err != nil || !ok || owner != "a" || failed {
		t.Fatalf("FetchLeaseHolder = %q, failed %v, ok %v, %v; want a's live lease", owner, failed, ok, err)
	}
	// A failed lease stays readable for those waiting on it, and is free to
	// take.
	if err := db.FailFetchLease("k", "b"); err != nil {
		t.Fatal(err)
	}
	if _, failed, _, _ := db.FetchLeaseHolder("k"); failed {
		t.Fatal("b failed a's lease")
	}
	if err := db.FailFetchLease("k", "a"); err != nil {
		t.Fatal(err)
	}
	if owner, failed, ok, err := db.FetchLeaseHolder("k"); err != nil || !ok || owner != "a" || !failed {
		t.Fatalf("FetchLeaseHolder = %q, failed %v, ok %v, %v; want a's failed lease", owner, failed, ok, err)
	}
	if ok, err := db.AcquireFetchLease("k", "b", time.Minute); err != nil || !ok {
		t.Fatalf("b could not take a's failed lease: %v, %v", ok, err)
	}
	if owner, failed, _, _ := db.FetchLeaseHolder("k"); owner != "b" || failed {
		t.Fatalf("after takeover: owner %q, failed %v", owner, failed)
	}
	if err := db.ReleaseFetchLease("k", "b"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := db.FetchLeaseHolder("k"); err != nil || ok {
		t.Fatalf("FetchLeaseHolder after release: ok %v, %v", ok, err)
	}
}
