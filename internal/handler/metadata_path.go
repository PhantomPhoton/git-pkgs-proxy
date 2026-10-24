package handler

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/git-pkgs/proxy/internal/database"
)

// metadataStoragePath builds the storage path for cached metadata. Cache
// keys are case-sensitive, but on a case-insensitive filesystem (APFS, NTFS)
// keys that differ only in case, such as Go's github.com/Sirupsen/logrus and
// github.com/sirupsen/logrus, would share one file. Keys may also hold names
// Windows reserves or end in a dot. So the path carries a hash of the exact
// key. Keys that already are lowercase SHA-256 hex digests are used as they
// are, so their existing entries stay valid.
func metadataStoragePath(ecosystem, cacheKey string) string {
	if !isSHA256Hex(cacheKey) {
		digest := sha256.Sum256([]byte(cacheKey))
		cacheKey = hex.EncodeToString(digest[:])
	}
	return legacyMetadataStoragePath(ecosystem, cacheKey)
}

// legacyMetadataStoragePath is where metadata was stored before paths were
// hashed: under the cache key itself.
func legacyMetadataStoragePath(ecosystem, cacheKey string) string {
	return "_metadata/" + ecosystem + "/" + cacheKey + "/metadata"
}

func isSHA256Hex(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// dropLegacyMetadataEntry ignores a cache row whose blob is still at its
// legacy, unhashed path. On a case-insensitive filesystem that blob may hold
// another key's document, so it is neither served nor revalidated; the fetch
// that follows stores the document at the hashed path. The old blob is
// queued for deletion, which waits until no row points at it.
func (p *Proxy) dropLegacyMetadataEntry(ecosystem, cacheKey string, entry *database.MetadataCacheEntry) *database.MetadataCacheEntry {
	if entry == nil {
		return nil
	}
	legacy := legacyMetadataStoragePath(ecosystem, cacheKey)
	if entry.StoragePath != legacy || legacy == metadataStoragePath(ecosystem, cacheKey) {
		return entry
	}
	if err := p.DB.QueuePendingDelete(legacy); err != nil {
		p.Logger.Warn("failed to queue legacy metadata blob for deletion", "ecosystem", ecosystem, "key", cacheKey, "error", err)
	}
	return nil
}
