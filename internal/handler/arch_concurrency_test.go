package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/git-pkgs/proxy/internal/storage"
)

// archHookStorage wraps a storage with hooks to force interleavings.
type archHookStorage struct {
	storage.Storage
	failStore    func(path string) error
	afterStore   func(path string)
	beforeDelete func(path string)
}

func (s *archHookStorage) Store(ctx context.Context, path string, r io.Reader) (int64, string, error) {
	if s.failStore != nil {
		if err := s.failStore(path); err != nil {
			return 0, "", err
		}
	}
	n, hash, err := s.Storage.Store(ctx, path, r)
	if s.afterStore != nil {
		s.afterStore(path)
	}
	return n, hash, err
}

func (s *archHookStorage) Delete(ctx context.Context, path string) error {
	if s.beforeDelete != nil {
		s.beforeDelete(path)
	}
	return s.Storage.Delete(ctx, path)
}

const archTestDB, archTestSig = "/custom/os/x86_64/custom.db", "/custom/os/x86_64/custom.db.sig"

// pauseOnce returns a hook that stops the first call for a path starting with
// prefix until release
// closes or a short timeout passes, so a test does not hang when locking
// keeps the other writer out.
func pauseOnce(prefix string, paused, release chan struct{}) func(string) {
	var fired atomic.Bool
	return func(p string) {
		if !strings.HasPrefix(p, prefix) || !fired.CompareAndSwap(false, true) {
			return
		}
		close(paused)
		select {
		case <-release:
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// waitPaused waits until a pauseOnce hook fired.
func waitPaused(t *testing.T, paused <-chan struct{}) {
	t.Helper()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("hook never fired")
	}
}

// waitBriefly waits for done, or gives up after a moment when a lock holds the
// call back.
func waitBriefly(done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
	}
}

// TestArchHandler_ReplicasWritingOutOfOrder covers two proxy replicas sharing
// database and storage, whose database writes finish out of order: the cache
// row must never describe other bytes than its blob, or every client gets the
// newest database with an older signature.
func TestArchHandler_ReplicasWritingOutOfOrder(t *testing.T) {
	upstream := newArchUpstream(t, map[string][]byte{archTestDB: []byte("db v1"), archTestSig: []byte("sig v1")})
	upstream.broken[archTestSig] = true
	proxy := newArchTestProxy(t, upstream)
	replicaA := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})
	replicaB := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})
	d := replicaA.database("custom", upstream.URL, archTestDB[1:])

	assertArchBody(t, serveArchRequest(replicaA, http.MethodGet, "/custom"+archTestDB, nil), "db v1", []byte("db v1"))
	upstream.mu.Lock()
	upstream.files[archTestDB], upstream.files[archTestSig] = []byte("db v2"), []byte("sig v2")
	upstream.broken[archTestSig] = false
	upstream.mu.Unlock()
	ageMetadata(t, proxy, replicaA, "custom", upstream.URL, archTestDB[1:])

	// Replica A's signature recovery stores db v2 and stalls before its row
	// update.
	paused, release := make(chan struct{}), make(chan struct{})
	proxy.Storage = &archHookStorage{Storage: proxy.Storage,
		afterStore: pauseOnce("_metadata/"+archEcosystem+"/"+d.dbKey+"/", paused, release)}
	recovered := make(chan struct{})
	go func() {
		serveArchRequest(replicaA, http.MethodGet, "/custom"+archTestSig, nil)
		close(recovered)
	}()
	waitPaused(t, paused)

	// Replica B pairs v2, then caches v3 whose signature fetch fails.
	assertArchBody(t, serveArchRequest(replicaB, http.MethodGet, "/custom"+archTestDB, nil), "B db v2", []byte("db v2"))
	assertArchBody(t, serveArchRequest(replicaB, http.MethodGet, "/custom"+archTestSig, nil), "B sig v2", []byte("sig v2"))
	upstream.mu.Lock()
	upstream.files[archTestDB], upstream.files[archTestSig] = []byte("db v3"), []byte("sig v3")
	upstream.broken[archTestSig] = true
	upstream.mu.Unlock()
	ageMetadata(t, proxy, replicaB, "custom", upstream.URL, archTestDB[1:])
	assertArchBody(t, serveArchRequest(replicaB, http.MethodGet, "/custom"+archTestDB, nil), "B db v3", []byte("db v3"))

	close(release)
	<-recovered

	db := serveArchRequest(replicaB, http.MethodGet, "/custom"+archTestDB, nil)
	sig := serveArchRequest(replicaB, http.MethodGet, "/custom"+archTestSig, nil)
	if sig.Code == http.StatusOK && strings.Replace(db.Body.String(), "db", "sig", 1) != sig.Body.String() {
		t.Errorf("%q served with %q", db.Body.String(), sig.Body.String())
	}
}

// TestArchHandler_FailedDatabaseWriteKeepsPair covers a database refresh whose
// cache write fails: the proxy must not hand out the new database while the
// cache still pairs the old one with its signature.
func TestArchHandler_FailedDatabaseWriteKeepsPair(t *testing.T) {
	upstream := newArchUpstream(t, map[string][]byte{archTestDB: []byte("db v1"), archTestSig: []byte("sig v1")})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})
	d := h.database("custom", upstream.URL, archTestDB[1:])
	var fail atomic.Bool
	proxy.Storage = &archHookStorage{Storage: proxy.Storage, failStore: func(path string) error {
		if fail.Load() && strings.HasPrefix(path, archBlobPath(d.dbKey, "")) {
			return errors.New("disk full")
		}
		return nil
	}}

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+archTestDB, nil), "db v1", []byte("db v1"))
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+archTestSig, nil), "sig v1", []byte("sig v1"))
	upstream.set(archTestDB, []byte("db v2"))
	upstream.set(archTestSig, []byte("sig v2"))
	ageMetadata(t, proxy, h, "custom", upstream.URL, archTestDB[1:])
	fail.Store(true)

	db := serveArchRequest(h, http.MethodGet, "/custom"+archTestDB, nil)
	sig := serveArchRequest(h, http.MethodGet, "/custom"+archTestSig, nil)
	if db.Code == http.StatusOK && sig.Code == http.StatusOK && strings.Replace(db.Body.String(), "db", "sig", 1) != sig.Body.String() {
		t.Errorf("%q served with %q", db.Body.String(), sig.Body.String())
	}
}

// TestArchHandler_ConcurrentSignatureWritesLeaveOneBlob covers two signature
// writes overlapping: afterwards exactly one signature blob is left and the
// cache row points at it.
func TestArchHandler_ConcurrentSignatureWritesLeaveOneBlob(t *testing.T) {
	d1, d2, d3 := archDigest([]byte("1")), archDigest([]byte("2")), archDigest([]byte("3"))

	for _, tt := range []struct {
		name  string
		hook  func(h *ArchHandler, d archDatabase, s *archHookStorage, paused, release chan struct{})
		first string // digest stored by the stalled write
		other string // digest stored by the write overtaking it
	}{
		{"stalled after store", func(_ *ArchHandler, d archDatabase, s *archHookStorage, paused, release chan struct{}) {
			s.afterStore = pauseOnce(archSignaturePath(d, d2), paused, release)
		}, d2, d3},
		{"stalled before delete", func(_ *ArchHandler, d archDatabase, s *archHookStorage, paused, release chan struct{}) {
			s.beforeDelete = pauseOnce(archSignaturePath(d, d1), paused, release)
		}, d2, d1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := newArchUpstream(t, map[string][]byte{})
			proxy := newArchTestProxy(t, upstream)
			mock := proxy.Storage.(*mockStorage)
			h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})
			d := h.database("custom", upstream.URL, archTestDB[1:])
			ctx := context.Background()
			h.storeSignature(ctx, d, d1, &upstreamMetadata{body: []byte("sig 1")})

			hooks := &archHookStorage{Storage: mock}
			proxy.Storage = hooks
			paused, release := make(chan struct{}), make(chan struct{})
			tt.hook(h, d, hooks, paused, release)

			stalled := make(chan struct{})
			go func() {
				h.storeSignature(ctx, d, tt.first, &upstreamMetadata{body: []byte("sig first")})
				close(stalled)
			}()
			waitPaused(t, paused)
			overtaking := make(chan struct{})
			go func() {
				h.storeSignature(ctx, d, tt.other, &upstreamMetadata{body: []byte("sig other")})
				close(overtaking)
			}()
			waitBriefly(overtaking)
			close(release)
			<-stalled
			<-overtaking

			row, _ := proxy.DB.GetMetadataCache(archEcosystem, d.sigKey)
			if _, err := mock.Open(ctx, row.StoragePath); err != nil {
				t.Errorf("row points at %s, which is gone: %v", row.StoragePath, err)
			}
			mock.mu.Lock()
			defer mock.mu.Unlock()
			var blobs []string
			for path := range mock.files {
				if strings.HasPrefix(path, archBlobPath(d.sigKey, "")) {
					blobs = append(blobs, path)
				}
			}
			if len(blobs) != 1 {
				t.Errorf("signature blobs = %v, want only the row's", blobs)
			}
		})
	}
}

// TestArchHandler_MissingSignatureRecheckedAfterTTL covers a 404 for the
// signature that was only transient: once the TTL is over the proxy asks
// again instead of answering 404 until the next database is published.
func TestArchHandler_MissingSignatureRecheckedAfterTTL(t *testing.T) {
	upstream := newArchUpstream(t, map[string][]byte{archTestDB: []byte("db v1")})
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+archTestDB, nil), "db", []byte("db v1"))
	if w := serveArchRequest(h, http.MethodGet, "/custom"+archTestSig, nil); w.Code != http.StatusNotFound {
		t.Fatalf("signature: status = %d, want 404", w.Code)
	}

	upstream.set(archTestSig, []byte("sig v1"))
	ageMetadata(t, proxy, h, "custom", upstream.URL, archTestDB[1:])
	ageMetadata(t, proxy, h, "custom", upstream.URL, archTestSig[1:])

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+archTestDB, nil), "db refreshed", []byte("db v1"))
	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+archTestSig, nil), "signature after TTL", []byte("sig v1"))
}

// TestArchHandler_SlowFetchDoesNotOverwriteNewerDatabase covers a signature
// recovery whose database fetch is still in flight while other clients
// refresh to newer databases. Its result must not replace the newer cached
// database, or a client holding that one gets an older signature.
func TestArchHandler_SlowFetchDoesNotOverwriteNewerDatabase(t *testing.T) {
	upstream := newArchUpstream(t, map[string][]byte{archTestDB: []byte("db v1"), archTestSig: []byte("sig v1")})
	upstream.broken[archTestSig] = true
	proxy := newArchTestProxy(t, upstream)
	h := NewArchHandler(proxy, "http://proxy.example", map[string]string{"custom": upstream.URL})

	assertArchBody(t, serveArchRequest(h, http.MethodGet, "/custom"+archTestDB, nil), "db v1", []byte("db v1"))
	upstream.mu.Lock()
	upstream.files[archTestDB], upstream.files[archTestSig] = []byte("db v2"), []byte("sig v2")
	upstream.broken[archTestSig] = false
	upstream.mu.Unlock()
	ageMetadata(t, proxy, h, "custom", upstream.URL, archTestDB[1:])

	// Client A's signature recovery has fetched db v2; the response is slow.
	paused, release := make(chan struct{}), make(chan struct{})
	upstream.mu.Lock()
	upstream.served = pauseOnce(archTestDB, paused, release)
	upstream.mu.Unlock()
	recovered := make(chan struct{})
	go func() {
		serveArchRequest(h, http.MethodGet, "/custom"+archTestSig, nil)
		close(recovered)
	}()
	waitPaused(t, paused)

	// Client B refreshes meanwhile, then the upstream moves to v3, whose
	// signature fails, and client C refreshes to it.
	b := make(chan struct{})
	go func() {
		serveArchRequest(h, http.MethodGet, "/custom"+archTestDB, nil)
		serveArchRequest(h, http.MethodGet, "/custom"+archTestSig, nil)
		close(b)
	}()
	waitBriefly(b)
	upstream.mu.Lock()
	upstream.files[archTestDB], upstream.files[archTestSig] = []byte("db v3"), []byte("sig v3")
	upstream.broken[archTestSig] = true
	upstream.mu.Unlock()
	ageMetadata(t, proxy, h, "custom", upstream.URL, archTestDB[1:])
	var cDB string
	c := make(chan struct{})
	go func() {
		cDB = serveArchRequest(h, http.MethodGet, "/custom"+archTestDB, nil).Body.String()
		close(c)
	}()
	waitBriefly(c)

	close(release)
	<-recovered
	<-b
	<-c

	if sig := serveArchRequest(h, http.MethodGet, "/custom"+archTestSig, nil); sig.Code == http.StatusOK && strings.Replace(cDB, "db", "sig", 1) != sig.Body.String() {
		t.Errorf("client holding %q got %q", cDB, sig.Body.String())
	}
	if got := serveArchRequest(h, http.MethodGet, "/custom"+archTestDB, nil).Body.String(); got < cDB {
		t.Errorf("cache went back to %q after serving %q", got, cDB)
	}
}
