package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MetadataDate is the Last-Modified a package's rewritten metadata is served
// with, and the ETag of the rewrite it dates.
//
// Rewritten metadata has no date of its own, so the proxy assigns one when a
// package starts serving a new rewrite. Keeping it in the database rather than
// in process memory is what lets replicas sharing the database agree on it: a
// date one replica gave out is never given by another to a different rewrite.
type MetadataDate struct {
	ETag         string
	LastModified time.Time
}

type metadataDateRow struct {
	ETag         string `db:"etag"`
	LastModified int64  `db:"last_modified"`
}

// GetMetadataDate returns the date recorded for a package's metadata as
// rewritten for proxyURL, or nil when there is none.
func (db *DB) GetMetadataDate(ecosystem, proxyURL, name string) (*MetadataDate, error) {
	var row metadataDateRow
	err := db.Get(&row, db.Rebind(`
		SELECT etag, last_modified FROM metadata_dates
		WHERE ecosystem = ? AND proxy_url = ? AND name = ?
	`), ecosystem, proxyURL, name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting metadata date: %w", err)
	}
	return &MetadataDate{ETag: row.ETag, LastModified: time.Unix(row.LastModified, 0).UTC()}, nil
}

// InsertMetadataDate records the first date of a package's metadata and
// reports whether it did; it leaves a date another replica recorded first.
func (db *DB) InsertMetadataDate(ecosystem, proxyURL, name string, date MetadataDate) (bool, error) {
	result, err := db.Exec(db.Rebind(`
		INSERT INTO metadata_dates (ecosystem, proxy_url, name, etag, last_modified)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (ecosystem, proxy_url, name) DO NOTHING
	`), ecosystem, proxyURL, name, date.ETag, date.LastModified.Unix())
	if err != nil {
		return false, fmt.Errorf("inserting metadata date: %w", err)
	}
	return affectedOne(result)
}

// SwapMetadataDate replaces a package's date with next if it is still old,
// and reports whether it did; it leaves a date another replica swapped in.
func (db *DB) SwapMetadataDate(ecosystem, proxyURL, name string, old, next MetadataDate) (bool, error) {
	result, err := db.Exec(db.Rebind(`
		UPDATE metadata_dates SET etag = ?, last_modified = ?
		WHERE ecosystem = ? AND proxy_url = ? AND name = ? AND etag = ? AND last_modified = ?
	`), next.ETag, next.LastModified.Unix(), ecosystem, proxyURL, name, old.ETag, old.LastModified.Unix())
	if err != nil {
		return false, fmt.Errorf("swapping metadata date: %w", err)
	}
	return affectedOne(result)
}

func affectedOne(result sql.Result) (bool, error) {
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting affected rows: %w", err)
	}
	return n == 1, nil
}
