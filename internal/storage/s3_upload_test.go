package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeS3 implements just enough of the S3 API for PutObject and multipart
// uploads, recording the size of each part it receives.
type fakeS3 struct {
	mu        sync.Mutex
	objects   map[string][]byte
	parts     map[int][]byte
	partSizes []int
	// discard drops bodies so a benchmark measures only the client.
	discard bool
}

func newFakeS3(t testing.TB) (*fakeS3, string) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, parts: map[int][]byte{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	url := "s3://bucket?endpoint=" + server.URL + "&region=us-east-1&use_path_style=true&anonymous=true&request_checksum_calculation=when_required"
	return f, url
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	q := r.URL.Query()
	if f.discard {
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body = http.NoBody
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.parts = map[int][]byte{}
		_, _ = fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><UploadId>upload</UploadId></InitiateMultipartUploadResult>`, key)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.parts[n] = body
		f.partSizes = append(f.partSizes, len(body))
		w.Header().Set("ETag", fmt.Sprintf(`"part%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var object []byte
		for _, n := range slices.Sorted(maps.Keys(f.parts)) {
			object = append(object, f.parts[n]...)
		}
		f.objects[key] = object
		_, _ = fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>bucket</Bucket><Key>%s</Key><ETag>"object"</ETag></CompleteMultipartUploadResult>`, key)
	case r.Method == http.MethodPut:
		f.objects[key] = body
		w.Header().Set("ETag", `"object"`)
	default:
		http.Error(w, "unsupported", http.StatusNotImplemented)
	}
}

func TestS3StoreUsesSmallUploadParts(t *testing.T) {
	fake, url := newFakeS3(t)
	store, err := OpenBucket(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	small := []byte("small object")
	large := bytes.Repeat([]byte("0123456789abcdef"), 20<<20/16)
	for name, data := range map[string][]byte{"small": small, "large": large} {
		size, _, err := store.Store(context.Background(), name, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("Store(%s): %v", name, err)
		}
		if size != int64(len(data)) || !bytes.Equal(fake.objects[name], data) {
			t.Errorf("Store(%s) wrote %d bytes, stored %d; want %d", name, size, len(fake.objects[name]), len(data))
		}
	}
	if len(fake.partSizes) == 0 {
		t.Fatal("large object was not uploaded in parts")
	}
	for _, n := range fake.partSizes {
		if n > uploadBufferSize {
			t.Errorf("part of %d bytes, want at most %d", n, uploadBufferSize)
		}
	}
}

func BenchmarkS3Store(b *testing.B) {
	fake, url := newFakeS3(b)
	fake.discard = true
	store, err := OpenBucket(context.Background(), url)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	data := bytes.Repeat([]byte("0123456789abcdef"), 32<<20/16)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, _, err := store.Store(context.Background(), "object", bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}
