package httpclient

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type gateHarness struct {
	gate  *RateLimitGate
	now   time.Time
	calls map[string]int
	// respond returns the status and Retry-After the fake upstream answers with.
	respond func(req *http.Request) (int, string)
}

func newGateHarness() *gateHarness {
	h := &gateHarness{
		now:   time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
		calls: map[string]int{},
		respond: func(*http.Request) (int, string) {
			return http.StatusOK, ""
		},
	}
	h.gate = NewRateLimitGate(roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		h.calls[req.URL.Host]++
		status, retryAfter := h.respond(req)
		resp := &http.Response{
			StatusCode: status,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}
		if retryAfter != "" {
			resp.Header.Set("Retry-After", retryAfter)
		}
		return resp, nil
	}))
	h.gate.now = func() time.Time { return h.now }
	return h
}

func (h *gateHarness) get(t *testing.T, rawURL string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := h.gate.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	return resp, err
}

func TestRateLimitGate_HoldsHostForRetryAfter(t *testing.T) {
	h := newGateHarness()
	h.respond = func(*http.Request) (int, string) { return http.StatusTooManyRequests, "785" }

	resp, err := h.get(t, "https://repo1.maven.org/maven2/a/b/maven-metadata.xml")
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("first request: resp=%v err=%v, want the upstream 429 passed through", resp, err)
	}

	h.respond = func(*http.Request) (int, string) { return http.StatusOK, "" }
	_, err = h.get(t, "https://repo1.maven.org/maven2/a/b/b-1.0.jar")
	var held *HeldError
	if !errors.As(err, &held) || !errors.Is(err, ErrUpstreamHeld) {
		t.Fatalf("second request error = %v, want *HeldError", err)
	}
	if held.Host != "repo1.maven.org" || held.RetryAfter != 785*time.Second {
		t.Errorf("held = %+v, want repo1.maven.org for 785s", held)
	}
	if h.calls["repo1.maven.org"] != 1 {
		t.Errorf("upstream calls = %d, want 1: held requests must not reach it", h.calls["repo1.maven.org"])
	}

	if _, err := h.get(t, "https://plugins.gradle.org/m2/x.pom"); err != nil {
		t.Errorf("other host: %v, want unaffected", err)
	}

	holds := h.gate.Holds()
	if until, ok := holds["repo1.maven.org"]; !ok || !until.Equal(h.now.Add(785*time.Second)) {
		t.Errorf("Holds() = %v, want repo1.maven.org until now+785s", holds)
	}
}

func TestRateLimitGate_ReleasesWhenHoldExpiresAndUpstreamServes(t *testing.T) {
	h := newGateHarness()
	h.respond = func(*http.Request) (int, string) { return http.StatusTooManyRequests, "60" }
	_, _ = h.get(t, "https://repo1.maven.org/x")

	h.now = h.now.Add(59 * time.Second)
	if _, err := h.get(t, "https://repo1.maven.org/x"); !errors.Is(err, ErrUpstreamHeld) {
		t.Fatalf("at 59s: err = %v, want still held", err)
	}

	h.now = h.now.Add(time.Second)
	h.respond = func(*http.Request) (int, string) { return http.StatusOK, "" }
	resp, err := h.get(t, "https://repo1.maven.org/x")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("at 60s: resp=%v err=%v, want the request sent", resp, err)
	}
	if len(h.gate.Holds()) != 0 {
		t.Errorf("Holds() = %v after a success, want empty", h.gate.Holds())
	}
}

func TestRateLimitGate_DefaultHoldDoublesWithoutHeader(t *testing.T) {
	h := newGateHarness()
	h.respond = func(*http.Request) (int, string) { return http.StatusTooManyRequests, "" }

	wantHolds := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, want := range wantHolds {
		_, _ = h.get(t, "https://repo1.maven.org/x")
		until := h.gate.Holds()["repo1.maven.org"]
		if got := until.Sub(h.now); got != want {
			t.Errorf("strike %d: hold = %s, want %s", i+1, got, want)
		}
		h.now = until
	}
}

func TestRateLimitGate_ConcurrentLimitedResponsesAreOneStrike(t *testing.T) {
	h := newGateHarness()
	h.respond = func(*http.Request) (int, string) { return http.StatusTooManyRequests, "" }

	// Two requests already in flight when the limit starts both see 429.
	// The gate only consults the hold before sending, so drive the base
	// directly for the second.
	_, _ = h.get(t, "https://repo1.maven.org/x")
	req, _ := http.NewRequest(http.MethodGet, "https://repo1.maven.org/y", nil)
	h.gate.hold(hostKey(req.URL), "")

	if got := h.gate.Holds()["repo1.maven.org"].Sub(h.now); got != time.Minute {
		t.Errorf("hold = %s after two concurrent 429s, want the single-strike %s", got, time.Minute)
	}
}

func TestRateLimitGate_CapsUpstreamRetryAfter(t *testing.T) {
	h := newGateHarness()
	h.respond = func(*http.Request) (int, string) { return http.StatusTooManyRequests, "86400" }
	_, _ = h.get(t, "https://repo1.maven.org/x")
	if got := h.gate.Holds()["repo1.maven.org"].Sub(h.now); got != maxHold {
		t.Errorf("hold = %s, want capped at %s", got, maxHold)
	}
}

func TestRateLimitGate_ServiceUnavailableNeedsRetryAfter(t *testing.T) {
	h := newGateHarness()
	h.respond = func(*http.Request) (int, string) { return http.StatusServiceUnavailable, "" }
	_, _ = h.get(t, "https://repo1.maven.org/x")
	if len(h.gate.Holds()) != 0 {
		t.Errorf("503 without Retry-After created hold %v, want none", h.gate.Holds())
	}

	h.respond = func(*http.Request) (int, string) { return http.StatusServiceUnavailable, "30" }
	_, _ = h.get(t, "https://repo1.maven.org/x")
	if got := h.gate.Holds()["repo1.maven.org"].Sub(h.now); got != 30*time.Second {
		t.Errorf("hold = %s, want 30s from the 503's Retry-After", got)
	}
}

func TestRateLimitGate_ReportsHoldsAndNilSafe(t *testing.T) {
	var nilGate *RateLimitGate
	if holds := nilGate.Holds(); holds != nil {
		t.Errorf("nil gate Holds() = %v, want nil", holds)
	}

	h := newGateHarness()
	var reported []string
	h.gate.OnHold = func(host string, retryAfter time.Duration) {
		reported = append(reported, host+"="+retryAfter.String())
	}
	h.respond = func(*http.Request) (int, string) { return http.StatusTooManyRequests, "10" }
	_, _ = h.get(t, "https://repo1.maven.org/x")
	if len(reported) != 1 || reported[0] != "repo1.maven.org=10s" {
		t.Errorf("OnHold reported %v, want [repo1.maven.org=10s]", reported)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"785", 785 * time.Second, true},
		{" 30 ", 30 * time.Second, true},
		{"0", 0, false},
		{"-5", 0, false},
		{"", 0, false},
		{"soon", 0, false},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, false},
	}
	for _, tt := range tests {
		got, ok := parseRetryAfter(tt.value, now)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseRetryAfter(%q) = (%s, %v), want (%s, %v)", tt.value, got, ok, tt.want, tt.ok)
		}
	}
}
