package database

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func seedSourcedArtifact(t *testing.T, db *DB, name, source string, size int64, fetched time.Time) {
	t.Helper()
	pkgPURL := "pkg:npm/" + name
	versionPURL := pkgPURL + "@1.0.0"
	if err := db.UpsertPackage(&Package{PURL: pkgPURL, Ecosystem: "npm", Name: name}); err != nil {
		t.Fatalf("UpsertPackage: %v", err)
	}
	if err := db.UpsertVersion(&Version{PURL: versionPURL, PackagePURL: pkgPURL}); err != nil {
		t.Fatalf("UpsertVersion: %v", err)
	}
	art := &Artifact{
		VersionPURL: versionPURL,
		Filename:    name + "-1.0.0.tgz",
		StoragePath: sql.NullString{String: "npm/" + name + "/1.0.0/" + name + "-1.0.0.tgz", Valid: true},
		Size:        sql.NullInt64{Int64: size, Valid: true},
		FetchedAt:   sql.NullTime{Time: fetched, Valid: true},
		Source:      source,
	}
	if source != SourceLocal {
		art.UpstreamURL = "https://registry.example/" + name + "-1.0.0.tgz"
	}
	if err := db.UpsertArtifact(art); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}
}

func TestArtifactSourceDefaultsToUpstream(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		seedSourcedArtifact(t, db, "cached", "", 100, time.Now())
		seedSourcedArtifact(t, db, "pushed", SourceLocal, 100, time.Now())

		cached, err := db.GetArtifact("pkg:npm/cached@1.0.0", "cached-1.0.0.tgz")
		if err != nil {
			t.Fatalf("GetArtifact(cached): %v", err)
		}
		if cached.Source != SourceUpstream {
			t.Errorf("cached artifact source = %q, want %q", cached.Source, SourceUpstream)
		}

		pushed, err := db.GetArtifact("pkg:npm/pushed@1.0.0", "pushed-1.0.0.tgz")
		if err != nil {
			t.Fatalf("GetArtifact(pushed): %v", err)
		}
		if pushed.Source != SourceLocal {
			t.Errorf("pushed artifact source = %q, want %q", pushed.Source, SourceLocal)
		}
		if pushed.UpstreamURL != "" {
			t.Errorf("pushed artifact upstream_url = %q, want empty", pushed.UpstreamURL)
		}
	})
}

func TestLocalArtifactsAreNotEvictable(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		seedSourcedArtifact(t, db, "cached", SourceUpstream, 1000, time.Now())
		seedSourcedArtifact(t, db, "pushed", SourceLocal, 5000, time.Now())

		total, err := db.GetTotalCacheSize()
		if err != nil {
			t.Fatalf("GetTotalCacheSize: %v", err)
		}
		if total != 1000 {
			t.Errorf("total cache size = %d, want 1000 (local artifacts excluded)", total)
		}

		lru, err := db.GetLeastRecentlyUsedArtifacts(10)
		if err != nil {
			t.Fatalf("GetLeastRecentlyUsedArtifacts: %v", err)
		}
		if len(lru) != 1 || lru[0].Filename != "cached-1.0.0.tgz" {
			t.Errorf("LRU artifacts = %+v, want only cached-1.0.0.tgz", lru)
		}
	})
}

func TestLocalArtifactsAreNotRetentionCandidates(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		old := time.Now().Add(-48 * time.Hour)
		cutoff := time.Now().Add(-24 * time.Hour)
		seedSourcedArtifact(t, db, "cached", SourceUpstream, 100, old)
		seedSourcedArtifact(t, db, "pushed", SourceLocal, 100, old)

		maxID, err := db.MaxArtifactID()
		if err != nil {
			t.Fatalf("MaxArtifactID: %v", err)
		}
		candidates, err := db.GetRetentionCandidates(0, maxID, cutoff)
		if err != nil {
			t.Fatalf("GetRetentionCandidates: %v", err)
		}
		got := candidateNames(candidates)
		if _, ok := got["pushed-1.0.0.tgz"]; ok {
			t.Error("local artifact returned as a retention candidate")
		}
		if _, ok := got["cached-1.0.0.tgz"]; !ok {
			t.Error("upstream artifact missing from retention candidates")
		}

		cleared, err := db.ClearExpiredArtifact("pkg:npm/pushed@1.0.0", "pushed-1.0.0.tgz", "npm/pushed/1.0.0/pushed-1.0.0.tgz", cutoff)
		if err != nil || cleared {
			t.Errorf("ClearExpiredArtifact(local) = %v, %v, want false, nil", cleared, err)
		}
	})
}

func TestMigrationAddsArtifactSource(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "existing.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sqlDB.Exec(schemaSQLite); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := sqlDB.Exec(`ALTER TABLE artifacts DROP COLUMN source`); err != nil {
		t.Fatalf("drop source column: %v", err)
	}
	if _, err := sqlDB.Exec(`DELETE FROM migrations WHERE name = '012_add_artifact_source'`); err != nil {
		t.Fatalf("forget migration: %v", err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO artifacts (version_purl, filename, upstream_url, created_at, updated_at)
		VALUES ('pkg:npm/old@1.0.0', 'old-1.0.0.tgz', 'https://registry.example/old-1.0.0.tgz', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema: %v", err)
	}

	art, err := db.GetArtifact("pkg:npm/old@1.0.0", "old-1.0.0.tgz")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if art.Source != SourceUpstream {
		t.Errorf("existing artifact source = %q, want %q", art.Source, SourceUpstream)
	}
}

func TestGetLocalArtifacts(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		seedSourcedArtifact(t, db, "request", SourceLocal, 100, time.Now())
		seedSourcedArtifact(t, db, "lodash", SourceLocal, 100, time.Now())
		seedSourcedArtifact(t, db, "express", SourceUpstream, 100, time.Now())

		local, err := db.GetLocalArtifacts("pkg:npm/request")
		if err != nil {
			t.Fatalf("GetLocalArtifacts: %v", err)
		}
		if len(local) != 1 || local[0].Filename != "request-1.0.0.tgz" {
			t.Fatalf("local artifacts of request = %+v, want only request-1.0.0.tgz", local)
		}

		upstreamOnly, err := db.GetLocalArtifacts("pkg:npm/express")
		if err != nil {
			t.Fatalf("GetLocalArtifacts(express): %v", err)
		}
		if len(upstreamOnly) != 0 {
			t.Errorf("local artifacts of express = %+v, want none", upstreamOnly)
		}
	})
}

func TestDeleteLocalArtifacts(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		seedSourcedArtifact(t, db, "request", SourceLocal, 100, time.Now())
		seedSourcedArtifact(t, db, "express", SourceUpstream, 100, time.Now())
		const pushedPath = "npm/request/1.0.0/request-1.0.0.tgz"

		deleted, err := db.DeleteLocalArtifacts("pkg:npm/request@1.0.0", "")
		if err != nil || deleted != 1 {
			t.Fatalf("DeleteLocalArtifacts(request) = %d, %v, want 1, nil", deleted, err)
		}
		art, err := db.GetArtifact("pkg:npm/request@1.0.0", "request-1.0.0.tgz")
		if err != nil || art != nil {
			t.Errorf("GetArtifact after delete = %+v, %v, want nil, nil", art, err)
		}
		var queued int
		if err := db.Get(&queued, db.Rebind(`SELECT COUNT(*) FROM pending_deletes WHERE path = ?`), pushedPath); err != nil || queued != 1 {
			t.Errorf("queued deletes for %s = %d, %v, want 1", pushedPath, queued, err)
		}

		deleted, err = db.DeleteLocalArtifacts("pkg:npm/express@1.0.0", "")
		if err != nil || deleted != 0 {
			t.Errorf("DeleteLocalArtifacts(upstream) = %d, %v, want 0, nil", deleted, err)
		}
		if art, err := db.GetArtifact("pkg:npm/express@1.0.0", "express-1.0.0.tgz"); err != nil || art == nil {
			t.Errorf("upstream artifact removed by DeleteLocalArtifacts: %+v, %v", art, err)
		}
	})
}

func TestLocalArtifactManifestAndIntegrity(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		if err := db.UpsertPackage(&Package{PURL: "pkg:npm/request", Ecosystem: "npm", Name: "request"}); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
		if err := db.UpsertVersion(&Version{
			PURL:        "pkg:npm/request@2.88.3",
			PackagePURL: "pkg:npm/request",
			Integrity:   sql.NullString{String: "sha512-abc", Valid: true},
		}); err != nil {
			t.Fatalf("UpsertVersion: %v", err)
		}
		const manifest = `{"name":"request","version":"2.88.3"}`
		if err := db.UpsertArtifact(&Artifact{
			VersionPURL: "pkg:npm/request@2.88.3",
			Filename:    "request-2.88.3.tgz",
			StoragePath: sql.NullString{String: "npm/request/2.88.3/x/request-2.88.3.tgz", Valid: true},
			ContentHash: sql.NullString{String: "00", Valid: true},
			FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
			Source:      SourceLocal,
			Manifest:    sql.NullString{String: manifest, Valid: true},
		}); err != nil {
			t.Fatalf("UpsertArtifact: %v", err)
		}

		local, err := db.GetLocalArtifacts("pkg:npm/request")
		if err != nil || len(local) != 1 {
			t.Fatalf("GetLocalArtifacts = %+v, %v", local, err)
		}
		if local[0].Manifest.String != manifest || local[0].Integrity.String != "sha512-abc" {
			t.Errorf("manifest/integrity = %q/%q", local[0].Manifest.String, local[0].Integrity.String)
		}
	})
}

func TestMigrationAddsArtifactManifest(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "existing.db")
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sqlDB.Exec(schemaSQLite); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := sqlDB.Exec(`ALTER TABLE artifacts DROP COLUMN manifest`); err != nil {
		t.Fatalf("drop manifest column: %v", err)
	}
	if _, err := sqlDB.Exec(`DELETE FROM migrations WHERE name = '013_add_artifact_manifest'`); err != nil {
		t.Fatalf("forget migration: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema: %v", err)
	}
	has, err := db.HasColumn("artifacts", "manifest")
	if err != nil || !has {
		t.Errorf("artifacts.manifest after migration = %v, %v", has, err)
	}
}

func TestUpsertPackageKeepsSource(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		pkg := &Package{PURL: "pkg:npm/request", Ecosystem: "npm", Name: "request"}
		if err := db.UpsertPackage(pkg); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
		if _, err := db.Exec(db.Rebind(`UPDATE packages SET source = ? WHERE purl = ?`), "deps.dev", pkg.PURL); err != nil {
			t.Fatalf("setting source: %v", err)
		}
		if err := db.UpsertPackage(pkg); err != nil {
			t.Fatalf("second UpsertPackage: %v", err)
		}
		got, err := db.GetPackageByPURL(pkg.PURL)
		if err != nil || got == nil {
			t.Fatalf("GetPackageByPURL: %+v, %v", got, err)
		}
		if got.Source.String != "deps.dev" {
			t.Errorf("source after upsert = %q, want deps.dev", got.Source.String)
		}
	})
}

func localPush(versionPURL, filename, storagePath string) *Artifact {
	return &Artifact{
		VersionPURL: versionPURL,
		Filename:    filename,
		StoragePath: sql.NullString{String: storagePath, Valid: true},
		ContentHash: sql.NullString{String: storagePath, Valid: true},
		Size:        sql.NullInt64{Int64: 1, Valid: true},
		FetchedAt:   sql.NullTime{Time: time.Now(), Valid: true},
	}
}

func TestRecordLocalArtifactNeverReplacesAPush(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		pkg := &Package{PURL: "pkg:npm/request", Ecosystem: "npm", Name: "request"}
		version := func(integrity string) *Version {
			return &Version{PURL: "pkg:npm/request@1.0.0", PackagePURL: pkg.PURL,
				Integrity: sql.NullString{String: integrity, Valid: true}}
		}

		first := localPush("pkg:npm/request@1.0.0", "request-1.0.0.tgz", "npm/request/first")
		if err := db.RecordLocalArtifact(pkg, version("sha512-first"), true, first); err != nil {
			t.Fatalf("first RecordLocalArtifact: %v", err)
		}
		second := localPush("pkg:npm/request@1.0.0", "request-1.0.0.tgz", "npm/request/second")
		if err := db.RecordLocalArtifact(pkg, version("sha512-second"), true, second); !errors.Is(err, ErrLocalArtifactExists) {
			t.Fatalf("second RecordLocalArtifact error = %v, want ErrLocalArtifactExists", err)
		}

		art, err := db.GetArtifact("pkg:npm/request@1.0.0", "request-1.0.0.tgz")
		if err != nil || art == nil || art.StoragePath.String != "npm/request/first" || art.Source != SourceLocal {
			t.Fatalf("artifact after refused push = %+v, %v, want the first push", art, err)
		}
		v, err := db.GetVersionByPURL("pkg:npm/request@1.0.0")
		if err != nil || v == nil || v.Integrity.String != "sha512-first" {
			t.Errorf("version integrity after refused push = %+v, %v, want the first push's", v, err)
		}
		var queued int
		if err := db.Get(&queued, db.Rebind(`SELECT COUNT(*) FROM pending_deletes`)); err != nil || queued != 0 {
			t.Errorf("queued deletes = %d, %v, want none", queued, err)
		}
	})
}

func TestRecordLocalArtifactReplacesUpstreamCopy(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		seedSourcedArtifact(t, db, "request", SourceUpstream, 100, time.Now())
		pkg := &Package{PURL: "pkg:npm/request", Ecosystem: "npm", Name: "request"}
		v := &Version{PURL: "pkg:npm/request@1.0.0", PackagePURL: pkg.PURL}

		pushed := localPush("pkg:npm/request@1.0.0", "request-1.0.0.tgz", "npm/request/pushed")
		if err := db.RecordLocalArtifact(pkg, v, true, pushed); err != nil {
			t.Fatalf("RecordLocalArtifact: %v", err)
		}
		art, err := db.GetArtifact("pkg:npm/request@1.0.0", "request-1.0.0.tgz")
		if err != nil || art == nil || art.Source != SourceLocal || art.StoragePath.String != "npm/request/pushed" {
			t.Fatalf("artifact = %+v, %v, want the push", art, err)
		}
		const upstreamPath = "npm/request/1.0.0/request-1.0.0.tgz"
		var queued int
		if err := db.Get(&queued, db.Rebind(`SELECT COUNT(*) FROM pending_deletes WHERE path = ?`), upstreamPath); err != nil || queued != 1 {
			t.Errorf("queued deletes for the upstream copy = %d, %v, want 1", queued, err)
		}
	})
}

func TestRecordLocalArtifactCanKeepTheVersionRecord(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		pkg := &Package{PURL: "pkg:generic/tools", Ecosystem: "generic", Name: "tools"}
		if err := db.UpsertPackage(pkg); err != nil {
			t.Fatalf("UpsertPackage: %v", err)
		}
		if err := db.UpsertVersion(&Version{PURL: "pkg:generic/tools@1.2.0", PackagePURL: pkg.PURL,
			License:   sql.NullString{String: "MIT", Valid: true},
			Integrity: sql.NullString{String: "sha256-upstream", Valid: true}}); err != nil {
			t.Fatalf("UpsertVersion: %v", err)
		}

		pushed := localPush("pkg:generic/tools@1.2.0", "tool.tar.gz", "generic/tools/pushed")
		if err := db.RecordLocalArtifact(pkg, &Version{PURL: "pkg:generic/tools@1.2.0", PackagePURL: pkg.PURL}, false, pushed); err != nil {
			t.Fatalf("RecordLocalArtifact: %v", err)
		}
		v, err := db.GetVersionByPURL("pkg:generic/tools@1.2.0")
		if err != nil || v == nil || v.Integrity.String != "sha256-upstream" || v.License.String != "MIT" {
			t.Errorf("version after push = %+v, %v, want it unchanged", v, err)
		}
	})
}

func TestRecordLocalArtifactConcurrentPushesHaveOneWinner(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		pkg := &Package{PURL: "pkg:generic/tools", Ecosystem: "generic", Name: "tools"}
		const pushes = 8
		start := make(chan struct{})
		errs := make([]error, pushes)
		var wg sync.WaitGroup
		for i := range pushes {
			wg.Go(func() {
				<-start
				v := &Version{PURL: "pkg:generic/tools@1.2.0", PackagePURL: pkg.PURL}
				a := localPush("pkg:generic/tools@1.2.0", "tool.tar.gz", fmt.Sprintf("generic/tools/push-%d", i))
				errs[i] = db.RecordLocalArtifact(pkg, v, false, a)
			})
		}
		close(start)
		wg.Wait()

		winners := 0
		for i, err := range errs {
			switch {
			case err == nil:
				winners++
			case !errors.Is(err, ErrLocalArtifactExists):
				t.Errorf("push %d error = %v, want ErrLocalArtifactExists", i, err)
			}
		}
		if winners != 1 {
			t.Errorf("%d pushes succeeded, want exactly 1", winners)
		}
	})
}

func TestRecordCachedArtifactNeverReplacesAPush(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		pkg := &Package{PURL: "pkg:npm/request", Ecosystem: "npm", Name: "request"}
		pushed := localPush("pkg:npm/request@1.0.0", "request-1.0.0.tgz", "npm/request/pushed")
		if err := db.RecordLocalArtifact(pkg, &Version{PURL: "pkg:npm/request@1.0.0", PackagePURL: pkg.PURL,
			Integrity: sql.NullString{String: "sha512-pushed", Valid: true}}, true, pushed); err != nil {
			t.Fatalf("RecordLocalArtifact: %v", err)
		}

		fetched := &Artifact{
			VersionPURL: "pkg:npm/request@1.0.0",
			Filename:    "request-1.0.0.tgz",
			UpstreamURL: "https://registry.example/request-1.0.0.tgz",
			StoragePath: sql.NullString{String: "npm/request/fetched", Valid: true},
		}
		err := db.RecordCachedArtifact(pkg, &Version{PURL: "pkg:npm/request@1.0.0", PackagePURL: pkg.PURL,
			Integrity: sql.NullString{String: "sha512-upstream", Valid: true}}, fetched)
		if !errors.Is(err, ErrLocalArtifactExists) {
			t.Fatalf("RecordCachedArtifact error = %v, want ErrLocalArtifactExists", err)
		}
		if err := db.UpsertArtifact(fetched); !errors.Is(err, ErrLocalArtifactExists) {
			t.Errorf("UpsertArtifact error = %v, want ErrLocalArtifactExists", err)
		}

		art, err := db.GetArtifact("pkg:npm/request@1.0.0", "request-1.0.0.tgz")
		if err != nil || art == nil || art.Source != SourceLocal || art.StoragePath.String != "npm/request/pushed" {
			t.Errorf("artifact = %+v, %v, want the push", art, err)
		}
		v, err := db.GetVersionByPURL("pkg:npm/request@1.0.0")
		if err != nil || v == nil || v.Integrity.String != "sha512-pushed" {
			t.Errorf("version = %+v, %v, want the push's integrity", v, err)
		}
	})
}
