package database

// CachedArtifactNames returns the names of the packages of ecosystem that
// have a cached artifact called filename, for protocols whose file names
// identify an artifact without its package name.
func (db *DB) CachedArtifactNames(ecosystem, filename string) ([]string, error) {
	var names []string
	query := db.Rebind(`
		SELECT DISTINCT packages.name
		FROM artifacts
		JOIN versions ON versions.purl = artifacts.version_purl
		JOIN packages ON packages.purl = versions.package_purl
		WHERE packages.ecosystem = ? AND artifacts.filename = ?
		  AND artifacts.storage_path IS NOT NULL AND artifacts.fetched_at IS NOT NULL
		ORDER BY packages.name
	`)
	if err := db.Select(&names, query, ecosystem, filename); err != nil {
		return nil, err
	}
	return names, nil
}
