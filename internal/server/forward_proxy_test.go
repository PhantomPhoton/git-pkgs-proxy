package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/git-pkgs/proxy/internal/config"
)

// routeThroughTestProxy starts a forward proxy that answers every request
// itself, points upstream clients at it, and resolves hostnames from hosts.
func routeThroughTestProxy(t *testing.T, hosts map[string]string) *atomic.Int32 {
	t.Helper()
	var received atomic.Int32
	forward := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		_, _ = w.Write([]byte("via proxy"))
	}))
	t.Cleanup(forward.Close)
	forwardURL, err := url.Parse(forward.URL)
	if err != nil {
		t.Fatal(err)
	}

	oldProxy, oldLookup := upstreamForwardProxy, lookupUpstreamHost
	upstreamForwardProxy = http.ProxyURL(forwardURL)
	lookupUpstreamHost = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if ip, ok := hosts[host]; ok {
			return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	t.Cleanup(func() { upstreamForwardProxy, lookupUpstreamHost = oldProxy, oldLookup })
	return &received
}

func TestUpstreamClientChecksDestinationBehindForwardProxy(t *testing.T) {
	received := routeThroughTestProxy(t, map[string]string{
		"metadata.example.test": "169.254.169.254",
		"intranet.example.test": "10.1.2.3",
		"allowed.example.test":  "10.1.2.4",
		"public.example.test":   "203.0.113.10",
	})
	// The forward proxy listens on loopback, so the client has to be allowed
	// to dial it.
	client := newUpstreamClient(config.UpstreamConfig{
		AllowLoopback:     true,
		AllowPrivateHosts: []string{"allowed.example.test", "10.9.9.9"},
	})

	for _, tt := range []struct {
		url     string
		blocked bool
	}{
		{"http://169.254.169.254/latest/meta-data/", true},
		{"https://169.254.169.254/latest/meta-data/", true},
		{"http://[fe80::1]/", true},
		{"http://metadata.example.test/", true},
		{"http://10.1.2.3/", true},
		{"http://intranet.example.test/", true},
		{"http://allowed.example.test/", false},
		{"http://10.9.9.9/", false},
		{"http://203.0.113.10/", false},
		{"http://public.example.test/", false},
		{"http://unresolvable.example.test/", false},
	} {
		before := received.Load()
		resp, err := client.Get(tt.url)
		if err == nil {
			_ = resp.Body.Close()
		}
		reached := received.Load() > before
		if tt.blocked {
			if err == nil || reached {
				t.Errorf("GET %s: err = %v, reached forward proxy = %v; want refused before the forward proxy", tt.url, err, reached)
			}
			continue
		}
		if err != nil || !reached {
			t.Errorf("GET %s: err = %v, reached forward proxy = %v; want it passed to the forward proxy", tt.url, err, reached)
		}
	}
}

func TestGuardForwardProxyKeepsDirectRequests(t *testing.T) {
	if guardForwardProxy(nil, upstreamSafeHTTPOptions(config.UpstreamConfig{})) != nil {
		t.Error("a transport without a proxy func got one")
	}
	proxyErr := errors.New("bad proxy config")
	guarded := guardForwardProxy(func(*http.Request) (*url.URL, error) { return nil, proxyErr }, upstreamSafeHTTPOptions(config.UpstreamConfig{}))
	req := httptest.NewRequest(http.MethodGet, "http://169.254.169.254/", nil)
	if _, err := guarded(req); !errors.Is(err, proxyErr) {
		t.Errorf("err = %v, want the proxy func's own error", err)
	}
	direct := guardForwardProxy(func(*http.Request) (*url.URL, error) { return nil, nil }, upstreamSafeHTTPOptions(config.UpstreamConfig{}))
	if proxyURL, err := direct(req); proxyURL != nil || err != nil {
		t.Errorf("direct request: proxy = %v, err = %v; want nil, nil so the dial gate checks it", proxyURL, err)
	}
}
