package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/git-pkgs/proxy/internal/database"
)

func TestRunExclusiveSQLiteRuns(t *testing.T) {
	db, err := database.Create(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("creating database: %v", err)
	}
	defer func() { _ = db.Close() }()
	s := &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ran := false
	s.runExclusive(context.Background(), "eviction", func() { ran = true })
	if !ran {
		t.Error("job did not run on SQLite")
	}
}

// TestRunExclusiveSkipsJobRunningElsewhere checks that a pass is skipped
// while another instance sharing the Postgres database runs the job, and
// runs once that instance is done.
func TestRunExclusiveSkipsJobRunningElsewhere(t *testing.T) {
	url := os.Getenv("PROXY_DATABASE_URL")
	if url == "" {
		t.Skip("PROXY_DATABASE_URL not set")
	}
	open := func() *Server {
		db, err := database.OpenPostgres(url)
		if err != nil {
			t.Fatalf("OpenPostgres: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return &Server{db: db, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	}
	a, b := open(), open()

	ranOnB := 0
	a.runExclusive(context.Background(), "eviction", func() {
		b.runExclusive(context.Background(), "eviction", func() { ranOnB++ })
		// A different job is not held up.
		b.runExclusive(context.Background(), "retention", func() { ranOnB += 10 })
	})
	if ranOnB != 10 {
		t.Fatalf("while a ran eviction, b ran %d (want only retention, 10)", ranOnB)
	}
	b.runExclusive(context.Background(), "eviction", func() { ranOnB++ })
	if ranOnB != 11 {
		t.Errorf("after a finished, b's eviction did not run")
	}
}
