package database

import (
	"testing"
	"time"
)

func TestUpstreamHolds(t *testing.T) {
	runWithBothDatabases(t, func(t *testing.T, db *DB) {
		now := time.UnixMilli(time.Now().UnixMilli())
		if err := db.PutUpstreamHold("registry.npmjs.org", now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		// A shorter hold reported later keeps the longer one.
		if err := db.PutUpstreamHold("registry.npmjs.org", now.Add(30*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := db.PutUpstreamHold("pypi.org", now.Add(10*time.Second)); err != nil {
			t.Fatal(err)
		}
		holds, err := db.UpstreamHolds(now)
		if err != nil {
			t.Fatal(err)
		}
		if len(holds) != 2 || !holds["registry.npmjs.org"].Equal(now.Add(time.Minute)) {
			t.Fatalf("holds = %v", holds)
		}

		// A longer one replaces it.
		if err := db.PutUpstreamHold("registry.npmjs.org", now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		holds, err = db.UpstreamHolds(now.Add(20 * time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if len(holds) != 1 || !holds["registry.npmjs.org"].Equal(now.Add(time.Hour)) {
			t.Fatalf("holds after pypi.org ended = %v", holds)
		}
		var rows int
		if err := db.Get(&rows, "SELECT COUNT(*) FROM upstream_holds"); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Errorf("rows = %d, want the ended hold deleted", rows)
		}
	})
}
