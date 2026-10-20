package database

import (
	"database/sql"
	"fmt"
	"time"
)

// LocalArtifact is a pushed artifact with the integrity of its version.
type LocalArtifact struct {
	Artifact
	Integrity sql.NullString `db:"integrity"`
}

// GetLocalArtifacts returns the artifacts pushed to the proxy for a package,
// ordered by version and filename.
func (db *DB) GetLocalArtifacts(packagePURL string) ([]LocalArtifact, error) {
	var artifacts []LocalArtifact
	query := db.Rebind(`
		SELECT a.id, a.version_purl, a.filename, a.upstream_url, a.storage_path, a.content_hash,
		       a.size, a.content_type, a.fetched_at, a.hit_count, a.last_accessed_at,
		       a.created_at, a.updated_at, a.source, a.manifest, v.integrity
		FROM artifacts a
		JOIN versions v ON v.purl = a.version_purl
		WHERE v.package_purl = ? AND a.source = 'local'
		ORDER BY a.version_purl, a.filename
	`)
	if err := db.Select(&artifacts, query, packagePURL); err != nil {
		return nil, err
	}
	return artifacts, nil
}

// DeleteLocalArtifacts removes the pushed artifacts of a version, or only the
// one named filename when it is set, and queues their stored objects for
// deletion in the same transaction, so a failed queue keeps the records
// instead of orphaning the objects. Artifacts cached from an upstream are left
// alone. It returns how many records it removed.
func (db *DB) DeleteLocalArtifacts(versionPURL, filename string) (int, error) {
	tx, err := db.Beginx()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	where := "version_purl = ? AND source = 'local'"
	args := []any{versionPURL}
	if filename != "" {
		where += " AND filename = ?"
		args = append(args, filename)
	}

	var paths []string
	selectQuery := db.Rebind(`SELECT storage_path FROM artifacts WHERE ` + where + ` AND storage_path IS NOT NULL`)
	if err := tx.Select(&paths, selectQuery, args...); err != nil {
		return 0, fmt.Errorf("reading local artifacts: %w", err)
	}

	res, err := tx.Exec(db.Rebind(`DELETE FROM artifacts WHERE `+where), args...)
	if err != nil {
		return 0, fmt.Errorf("deleting local artifacts: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}

	queuedAt := time.Now().UTC()
	for _, path := range paths {
		if _, err := tx.Exec(db.Rebind(queuePendingDeleteQuery), path, queuedAt); err != nil {
			return 0, fmt.Errorf("queueing %s for deletion: %w", path, err)
		}
	}
	return int(deleted), tx.Commit()
}
