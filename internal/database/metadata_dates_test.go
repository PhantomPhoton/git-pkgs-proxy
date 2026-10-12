package database

import (
	"path/filepath"
	"testing"
	"time"
)

const testProxyURL = "http://proxy.example"

func TestMetadataDateMissing(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		got, err := db.GetMetadataDate(testEcosystemNPM, testProxyURL, "missing")
		if err != nil || got != nil {
			t.Errorf("GetMetadataDate() = %+v, %v; want nil, nil", got, err)
		}
	})
}

func TestMetadataDateInsertOnce(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		first := MetadataDate{ETag: `"a"`, LastModified: time.Date(2026, 10, 10, 12, 0, 1, 0, time.UTC)}
		second := MetadataDate{ETag: `"b"`, LastModified: first.LastModified.Add(time.Second)}

		inserted, err := db.InsertMetadataDate(testEcosystemNPM, testProxyURL, "left-pad", first)
		if err != nil || !inserted {
			t.Fatalf("first InsertMetadataDate() = %v, %v; want true", inserted, err)
		}
		inserted, err = db.InsertMetadataDate(testEcosystemNPM, testProxyURL, "left-pad", second)
		if err != nil || inserted {
			t.Fatalf("second InsertMetadataDate() = %v, %v; want false, the row exists", inserted, err)
		}

		got, err := db.GetMetadataDate(testEcosystemNPM, testProxyURL, "left-pad")
		if err != nil || got == nil || *got != first {
			t.Errorf("GetMetadataDate() = %+v, %v; want %+v", got, err, first)
		}
	})
}

// A package's date is kept per proxy URL: each one rewrites the document to
// different download URLs, so each serves a different representation.
func TestMetadataDateKeyedByProxyURL(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		date := MetadataDate{ETag: `"a"`, LastModified: time.Date(2026, 10, 10, 12, 0, 1, 0, time.UTC)}
		if _, err := db.InsertMetadataDate(testEcosystemNPM, testProxyURL, "left-pad", date); err != nil {
			t.Fatal(err)
		}
		inserted, err := db.InsertMetadataDate(testEcosystemNPM, "http://other.example", "left-pad", date)
		if err != nil || !inserted {
			t.Errorf("InsertMetadataDate() for another proxy URL = %v, %v; want true", inserted, err)
		}
	})
}

// SwapMetadataDate is a compare-and-swap: it only replaces the date it was
// given, so of two replicas swapping from the same date only one succeeds.
func TestMetadataDateSwap(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		a := MetadataDate{ETag: `"a"`, LastModified: time.Date(2026, 10, 10, 12, 0, 1, 0, time.UTC)}
		b := MetadataDate{ETag: `"b"`, LastModified: a.LastModified.Add(time.Second)}
		c := MetadataDate{ETag: `"c"`, LastModified: b.LastModified.Add(time.Second)}
		if _, err := db.InsertMetadataDate(testEcosystemNPM, testProxyURL, "left-pad", a); err != nil {
			t.Fatal(err)
		}

		swapped, err := db.SwapMetadataDate(testEcosystemNPM, testProxyURL, "left-pad", a, b)
		if err != nil || !swapped {
			t.Fatalf("SwapMetadataDate(a, b) = %v, %v; want true", swapped, err)
		}
		swapped, err = db.SwapMetadataDate(testEcosystemNPM, testProxyURL, "left-pad", a, c)
		if err != nil || swapped {
			t.Fatalf("SwapMetadataDate(a, c) after a was replaced = %v, %v; want false", swapped, err)
		}

		got, err := db.GetMetadataDate(testEcosystemNPM, testProxyURL, "left-pad")
		if err != nil || got == nil || *got != b {
			t.Errorf("GetMetadataDate() = %+v, %v; want %+v", got, err, b)
		}
	})
}

// Dates are whole seconds in UTC whatever the local zone, so every replica
// reads back the date another one wrote.
func TestMetadataDateIsUTCSeconds(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("time zone data unavailable: %v", err)
	}
	prev := time.Local
	time.Local = tokyo
	t.Cleanup(func() { time.Local = prev })

	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		written := time.Date(2026, 10, 10, 21, 0, 1, 700_000_000, tokyo)
		if _, err := db.InsertMetadataDate(testEcosystemNPM, testProxyURL, "tz", MetadataDate{ETag: `"a"`, LastModified: written}); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetMetadataDate(testEcosystemNPM, testProxyURL, "tz")
		if err != nil || got == nil {
			t.Fatalf("GetMetadataDate() = %+v, %v", got, err)
		}
		if want := time.Date(2026, 10, 10, 12, 0, 1, 0, time.UTC); !got.LastModified.Equal(want) || got.LastModified.Location() != time.UTC {
			t.Errorf("LastModified read back as %v, want %v", got.LastModified, want)
		}
	})
}

func TestMetadataDatesTableCreatedByMigration(t *testing.T) {
	db, err := Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("DROP TABLE metadata_dates"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM migrations WHERE name = '011_add_metadata_dates'"); err != nil {
		t.Fatal(err)
	}

	if err := db.MigrateSchema(); err != nil {
		t.Fatalf("MigrateSchema() error = %v", err)
	}
	if has, err := db.HasTable("metadata_dates"); err != nil || !has {
		t.Errorf("HasTable(metadata_dates) = %v, %v; want true after migration", has, err)
	}
}
