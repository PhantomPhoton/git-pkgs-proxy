package database

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// benchArtifacts is the artifact count the read pool benchmarks seed: about
// what a busy cache holds, and enough for the stats queries to take a while.
const (
	benchPackages  = 30_000
	benchVersions  = 100_000
	benchArtifacts = 300_000
)

// seedBenchDB creates a database at path with benchArtifacts artifacts
// spread over three ecosystems, written in one statement per table.
func seedBenchDB(b *testing.B, path string) {
	b.Helper()
	db, err := Create(path)
	if err != nil {
		b.Fatalf("Create: %v", err)
	}
	defer func() { _ = db.Close() }()
	stmts := []string{
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i+1 < %d)
			INSERT INTO packages (purl, ecosystem, name, created_at, updated_at)
			SELECT 'pkg:' || e || '/p' || i, e, 'p' || i, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
			FROM (SELECT i, CASE i %% 3 WHEN 0 THEN 'npm' WHEN 1 THEN 'pypi' ELSE 'cargo' END AS e FROM n)`, benchPackages),
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i+1 < %d)
			INSERT INTO versions (purl, package_purl, integrity, created_at, updated_at)
			SELECT p.purl || '@' || n.i, p.purl, 'sha256-x', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
			FROM n JOIN packages p ON p.id = (n.i %% %d) + 1`, benchVersions, benchPackages),
		fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i+1 < %d)
			INSERT INTO artifacts (version_purl, filename, upstream_url, storage_path, content_hash, size,
			                       content_type, fetched_at, hit_count, last_accessed_at, created_at, updated_at)
			SELECT v.purl, 'f' || n.i, 'https://example.org/' || n.i, 'blobs/' || n.i, 'h', 1000 + n.i,
			       'application/octet-stream', CURRENT_TIMESTAMP, n.i %% 7, CURRENT_TIMESTAMP,
			       CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
			FROM n JOIN versions v ON v.id = (n.i %% %d) + 1`, benchArtifacts, benchVersions),
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			b.Fatalf("seeding: %v", err)
		}
	}
}

// openBenchDB opens path. With oneConn it routes reads to the writer, as
// before the read pool existed.
func openBenchDB(b *testing.B, path string, oneConn bool) *DB {
	b.Helper()
	db, err := Open(path)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	if oneConn {
		_ = db.reader.Close()
		db.reader = db.DB
	}
	b.Cleanup(func() { _ = db.Close() })
	return db
}

// benchKey returns the lookup arguments for artifact i, as seeded above.
func benchKey(i int) (pkgPURL, versionPURL, filename string) {
	v := i % benchVersions
	p := v % benchPackages
	eco := [...]string{"npm", "pypi", "cargo"}[p%3]
	pkgPURL = fmt.Sprintf("pkg:%s/p%d", eco, p)
	return pkgPURL, fmt.Sprintf("%s@%d", pkgPURL, v), fmt.Sprintf("f%d", i)
}

func lookup(b *testing.B, db *DB, i int) {
	pkgPURL, versionPURL, filename := benchKey(i % benchArtifacts)
	art, err := db.GetCachedArtifact(pkgPURL, versionPURL, filename)
	if err != nil || art == nil {
		b.Fatalf("GetCachedArtifact(%s, %s) = %v, %v", versionPURL, filename, art, err)
	}
}

var poolModes = []struct {
	name    string
	oneConn bool
}{
	{"one-connection", true},
	{"read-pool", false},
}

// BenchmarkSQLiteParallelLookups runs GetCachedArtifact from parallel
// goroutines, as concurrent downloads do.
func BenchmarkSQLiteParallelLookups(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	seedBenchDB(b, path)
	for _, mode := range poolModes {
		b.Run(mode.name, func(b *testing.B) {
			db := openBenchDB(b, path, mode.oneConn)
			var next atomic.Int64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					lookup(b, db, int(next.Add(7919)))
				}
			})
		})
	}
}

// BenchmarkSQLiteLookupDuringStats runs GetCachedArtifact one at a time
// while another goroutine keeps running GetEcosystemStats and GetCacheStats,
// as the metrics updater and the dashboard do. It reports the slowest lookup.
func BenchmarkSQLiteLookupDuringStats(b *testing.B) {
	path := filepath.Join(b.TempDir(), "bench.db")
	seedBenchDB(b, path)
	for _, mode := range poolModes {
		b.Run(mode.name, func(b *testing.B) {
			db := openBenchDB(b, path, mode.oneConn)
			stop := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if _, err := db.GetEcosystemStats(); err != nil {
						b.Errorf("GetEcosystemStats: %v", err)
						return
					}
					if _, err := db.GetCacheStats(); err != nil {
						b.Errorf("GetCacheStats: %v", err)
						return
					}
				}
			}()

			var slowest time.Duration
			b.ResetTimer()
			for i := range b.N {
				start := time.Now()
				lookup(b, db, i*7919)
				slowest = max(slowest, time.Since(start))
			}
			b.StopTimer()
			close(stop)
			wg.Wait()
			b.ReportMetric(float64(slowest.Microseconds())/1000, "max-ms")
		})
	}
}
