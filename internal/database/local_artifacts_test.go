package database

import (
	"database/sql"
	"path/filepath"
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

		deleted, err := db.DeleteLocalArtifacts("pkg:npm/request@1.0.0")
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

		deleted, err = db.DeleteLocalArtifacts("pkg:npm/express@1.0.0")
		if err != nil || deleted != 0 {
			t.Errorf("DeleteLocalArtifacts(upstream) = %d, %v, want 0, nil", deleted, err)
		}
		if art, err := db.GetArtifact("pkg:npm/express@1.0.0", "express-1.0.0.tgz"); err != nil || art == nil {
			t.Errorf("upstream artifact removed by DeleteLocalArtifacts: %+v, %v", art, err)
		}
	})
}
