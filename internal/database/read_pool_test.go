package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedOnePackage(t *testing.T, db *DB) {
	t.Helper()
	pkg := &Package{PURL: "pkg:npm/left-pad", Ecosystem: "npm", Name: "left-pad"}
	ver := &Version{PURL: "pkg:npm/left-pad@1.3.0", PackagePURL: pkg.PURL}
	art := &Artifact{
		VersionPURL: ver.PURL,
		Filename:    "left-pad-1.3.0.tgz",
		UpstreamURL: "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz",
		StoragePath: sql.NullString{String: "npm/left-pad/1.3.0/left-pad-1.3.0.tgz", Valid: true},
		Size:        sql.NullInt64{Int64: 10, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
	if err := db.RecordCachedArtifact(pkg, ver, art); err != nil {
		t.Fatalf("RecordCachedArtifact: %v", err)
	}
}

func TestSQLiteUsesSeparateReadPool(t *testing.T) {
	db := newTestDB(t)
	if db.reader == db.DB {
		t.Fatal("file database shares one pool for reads and writes")
	}
	if got, want := db.reader.Stats().MaxOpenConnections, sqliteReaderConns(); got != want {
		t.Errorf("reader MaxOpenConnections = %d, want %d", got, want)
	}
	if got := db.DB.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("writer MaxOpenConnections = %d, want 1", got)
	}
}

func TestSQLiteReadPoolRejectsWrites(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.reader.Exec(`INSERT INTO pending_deletes (path, queued_at) VALUES ('x', CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("write on the reader pool succeeded")
	} else if !strings.Contains(err.Error(), "readonly") {
		t.Fatalf("write on the reader pool failed with %v, want a read-only error", err)
	}

	// Get goes to the reader, so a write sent through it must fail too
	// rather than run.
	var path string
	err := db.Get(&path, `INSERT INTO pending_deletes (path, queued_at) VALUES ('y', CURRENT_TIMESTAMP) RETURNING path`)
	if err == nil {
		t.Fatal("INSERT ... RETURNING through Get succeeded")
	}

	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM pending_deletes`); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("pending_deletes has %d rows after rejected writes, want 0", n)
	}
}

// pragmaInt reads an integer pragma on conn.
func pragmaInt(t *testing.T, conn *sql.Conn, name string) int {
	t.Helper()
	var v int
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func pragmaString(t *testing.T, conn *sql.Conn, name string) string {
	t.Helper()
	var v string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

// TestSQLitePragmasOnEveryConnection holds every connection of both pools at
// once, so each one is a separate SQLite connection, and checks that each
// got the settings from the DSN.
func TestSQLitePragmasOnEveryConnection(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("writer conn: %v", err)
	}
	defer func() { _ = writer.Close() }()
	if got := pragmaInt(t, writer, "busy_timeout"); got != 5000 {
		t.Errorf("writer busy_timeout = %d, want 5000", got)
	}
	if got := pragmaString(t, writer, "journal_mode"); got != "wal" {
		t.Errorf("writer journal_mode = %q, want wal", got)
	}
	if got := pragmaInt(t, writer, "synchronous"); got != 1 {
		t.Errorf("writer synchronous = %d, want 1 (NORMAL)", got)
	}
	if got := pragmaInt(t, writer, "query_only"); got != 0 {
		t.Errorf("writer query_only = %d, want 0", got)
	}

	n := sqliteReaderConns()
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := range n {
		c, err := db.reader.Conn(ctx)
		if err != nil {
			t.Fatalf("reader conn %d: %v", i, err)
		}
		conns = append(conns, c)
		if got := pragmaInt(t, c, "busy_timeout"); got != 5000 {
			t.Errorf("reader %d busy_timeout = %d, want 5000", i, got)
		}
		if got := pragmaString(t, c, "journal_mode"); got != "wal" {
			t.Errorf("reader %d journal_mode = %q, want wal", i, got)
		}
		if got := pragmaInt(t, c, "query_only"); got != 1 {
			t.Errorf("reader %d query_only = %d, want 1", i, got)
		}
	}
	if got := db.reader.Stats().OpenConnections; got != n {
		t.Errorf("reader pool has %d open connections, want %d", got, n)
	}
}

// TestSQLiteReadNotBlockedBySlowRead holds one reader connection in an open
// read, as a long stats scan does, and a write transaction on the writer.
// A lookup must still complete, and must not see the uncommitted write.
func TestSQLiteReadNotBlockedBySlowRead(t *testing.T) {
	db := newTestDB(t)
	seedOnePackage(t, db)

	rows, err := db.Query(`SELECT purl FROM packages`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("no package row")
	}

	// Bounded, so with a single shared connection the test fails instead
	// of waiting forever for the connection the open read holds.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTxx while a read is open: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO pending_deletes (path, queued_at) VALUES ('uncommitted', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert in tx: %v", err)
	}

	type result struct {
		art     *CachedArtifact
		pending int
		err     error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		r.art, r.err = db.GetCachedArtifact("pkg:npm/left-pad", "pkg:npm/left-pad@1.3.0", "left-pad-1.3.0.tgz")
		if r.err == nil {
			r.err = db.GetContext(ctx, &r.pending, `SELECT COUNT(*) FROM pending_deletes`)
		}
		done <- r
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("lookup: %v", r.err)
		}
		if r.art == nil {
			t.Fatal("lookup found no artifact")
		}
		if r.pending != 0 {
			t.Errorf("reader saw %d uncommitted pending deletes, want 0", r.pending)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lookup blocked behind an open read and a write transaction")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var pending int
	if err := db.Get(&pending, `SELECT COUNT(*) FROM pending_deletes`); err != nil {
		t.Fatalf("count after commit: %v", err)
	}
	if pending != 1 {
		t.Errorf("reader saw %d pending deletes after commit, want 1", pending)
	}
}

// TestSQLiteReadAfterWrite checks that a read right after a write, on the
// other pool, sees it.
func TestSQLiteReadAfterWrite(t *testing.T) {
	db := newTestDB(t)
	for i := range 50 {
		path := fmt.Sprintf("npm/p/%d.tgz", i)
		if err := db.QueuePendingDelete(path); err != nil {
			t.Fatalf("QueuePendingDelete: %v", err)
		}
		var n int
		if err := db.Get(&n, db.Rebind(`SELECT COUNT(*) FROM pending_deletes WHERE path = ?`), path); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Fatalf("write %d not visible to the reader", i)
		}
	}
}

func TestSQLiteMemoryDatabaseSharesOneConnection(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if db.reader != db.DB {
		t.Fatal("in-memory database got a separate reader pool")
	}
	if err := db.CreateSchema(); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	seedOnePackage(t, db)
	art, err := db.GetCachedArtifact("pkg:npm/left-pad", "pkg:npm/left-pad@1.3.0", "left-pad-1.3.0.tgz")
	if err != nil || art == nil {
		t.Fatalf("GetCachedArtifact = %v, %v", art, err)
	}
}

func TestSQLiteCloseClosesBothPools(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "close.db"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	reader := db.reader
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := reader.Ping(); err == nil {
		t.Fatal("reader pool still open after Close")
	}
	if err := db.Ping(); err == nil {
		t.Fatal("writer pool still open after Close")
	}
}

func TestPostgresSharesOnePool(t *testing.T) {
	url := os.Getenv("PROXY_DATABASE_URL")
	if url == "" {
		t.Skip("PROXY_DATABASE_URL not set, skipping postgres test")
	}
	db, err := OpenPostgres(url)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	defer func() { _ = db.Close() }()
	if db.reader != db.DB {
		t.Fatal("postgres got a separate reader pool")
	}
}

func TestIsMemoryPath(t *testing.T) {
	for path, want := range map[string]bool{
		"":                           true,
		":memory:":                   true,
		"file::memory:":              true,
		"file::memory:?cache=shared": true,
		"file:cache?mode=memory":     true,
		"/var/lib/proxy/cache.db":    false,
		"cache.db":                   false,
	} {
		if got := isMemoryPath(path); got != want {
			t.Errorf("isMemoryPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestSQLiteExistingRollbackJournalDatabase opens a database file left in
// rollback-journal mode, as an older or foreign tool may leave it. The
// writer must switch it to WAL before readers open it, or reads would block
// on writes.
func TestSQLiteExistingRollbackJournalDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedOnePackage(t, db)
	if _, err := db.Exec("PRAGMA journal_mode = DELETE"); err != nil {
		t.Fatalf("leaving WAL: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err = Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var mode string
	if err := db.reader.Get(&mode, "PRAGMA journal_mode"); err != nil || mode != "wal" {
		t.Fatalf("reader journal_mode = %q, %v; want wal", mode, err)
	}
	art, err := db.GetCachedArtifact("pkg:npm/left-pad", "pkg:npm/left-pad@1.3.0", "left-pad-1.3.0.tgz")
	if err != nil || art == nil {
		t.Fatalf("GetCachedArtifact = %v, %v", art, err)
	}
}

// TestSQLiteConcurrentReadsAndWrites runs the writes a busy proxy makes
// (recording artifacts and hits) alongside request lookups and stats scans.
// None may fail with a busy, locked or read-only error.
func TestSQLiteConcurrentReadsAndWrites(t *testing.T) {
	db := newTestDB(t)
	seedOnePackage(t, db)
	deadline := time.Now().Add(time.Second)
	errs := make(chan error, 64)
	var wg sync.WaitGroup
	run := func(f func(i int) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				if err := f(i); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for w := range 2 {
		run(func(i int) error {
			name := fmt.Sprintf("pkg-%d-%d", w, i)
			pkg := &Package{PURL: "pkg:npm/" + name, Ecosystem: "npm", Name: name}
			ver := &Version{PURL: pkg.PURL + "@1.0.0", PackagePURL: pkg.PURL}
			art := &Artifact{
				VersionPURL: ver.PURL,
				Filename:    name + ".tgz",
				UpstreamURL: "https://registry.npmjs.org/" + name,
				StoragePath: sql.NullString{String: "npm/" + name, Valid: true},
				Size:        sql.NullInt64{Int64: 10, Valid: true},
				FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
			}
			return db.RecordCachedArtifact(pkg, ver, art)
		})
	}
	run(func(int) error { return db.RecordArtifactHit("pkg:npm/left-pad@1.3.0", "left-pad-1.3.0.tgz") })
	for range 4 {
		run(func(int) error {
			art, err := db.GetCachedArtifact("pkg:npm/left-pad", "pkg:npm/left-pad@1.3.0", "left-pad-1.3.0.tgz")
			if err == nil && art == nil {
				err = errors.New("seeded artifact not found")
			}
			return err
		})
	}
	run(func(int) error {
		if _, err := db.GetCacheStats(); err != nil {
			return err
		}
		_, err := db.GetEcosystemStats()
		return err
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
