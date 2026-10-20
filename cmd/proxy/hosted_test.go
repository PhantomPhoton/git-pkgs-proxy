package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordedRequest struct {
	method, path, query, authorization, body, expect string
	contentLength                                    int64
}

func hostedTestServer(t *testing.T, status int, response string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	recorded := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*recorded = recordedRequest{
			method:        r.Method,
			path:          r.URL.EscapedPath(),
			query:         r.URL.RawQuery,
			authorization: r.Header.Get("Authorization"),
			body:          string(body),
			expect:        r.Header.Get("Expect"),
			contentLength: r.ContentLength,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	return srv, recorded
}

func hostedEnv(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func writeArchive(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing archive: %v", err)
	}
	return path
}

func TestRunPush(t *testing.T) {
	srv, recorded := hostedTestServer(t, http.StatusCreated,
		`{"ecosystem":"npm","name":"@types/node","version":"20.1.0","filename":"node-20.1.0.tgz","sha256":"abc123","size":7}`)
	archive := writeArchive(t, "node-20.1.0.tgz", "tarball")
	var stdout bytes.Buffer

	err := runPush([]string{"-url", srv.URL, "npm", "@types/node", "20.1.0", archive},
		hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok"}), &stdout, srv.Client())
	if err != nil {
		t.Fatalf("runPush: %v", err)
	}
	if recorded.method != http.MethodPut || recorded.path != "/api/hosted/npm/@types/node/20.1.0" {
		t.Errorf("request = %s %s", recorded.method, recorded.path)
	}
	// sha256 of "tarball", so the proxy can check the upload.
	const tarballSHA256 = "db4b4d0d1cb480bf9aeea253771c00febe627f236765fa37d6a5614f079a3aa0"
	if recorded.authorization != "Bearer tok" || recorded.body != "tarball" || recorded.query != "sha256="+tarballSHA256 {
		t.Errorf("authorization/body/query = %q/%q/%q", recorded.authorization, recorded.body, recorded.query)
	}
	if recorded.expect != "100-continue" || recorded.contentLength != int64(len("tarball")) {
		t.Errorf("expect/content length = %q/%d, want 100-continue and the file size", recorded.expect, recorded.contentLength)
	}
	if out := stdout.String(); !strings.Contains(out, "node-20.1.0.tgz") || !strings.Contains(out, "abc123") {
		t.Errorf("output = %q, want filename and sha256", out)
	}
}

func TestRunPushGenericNamesTheFile(t *testing.T) {
	srv, recorded := hostedTestServer(t, http.StatusCreated,
		`{"ecosystem":"generic","name":"tools","version":"1.2.0","filename":"tool-linux-arm64.tar.gz","sha256":"abc123","size":7}`)
	archive := writeArchive(t, "tool-linux-arm64.tar.gz", "tarball")

	err := runPush([]string{"-url", srv.URL, "generic", "tools", "1.2.0", archive},
		hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok"}), io.Discard, srv.Client())
	if err != nil {
		t.Fatalf("runPush: %v", err)
	}
	if recorded.path != "/api/hosted/generic/tools/1.2.0/tool-linux-arm64.tar.gz" {
		t.Errorf("path = %q, want the file name appended", recorded.path)
	}
}

func TestRunPushErrors(t *testing.T) {
	srv, _ := hostedTestServer(t, http.StatusBadRequest, `{"code":"BAD_REQUEST","message":"pushing packages is not supported for this ecosystem: cargo"}`)
	archive := writeArchive(t, "serde-1.0.200.crate", "crate")

	err := runPush([]string{"-url", srv.URL, "cargo", "serde", "1.0.200", archive}, hostedEnv(nil), io.Discard, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "PROXY_HOSTED_TOKEN") {
		t.Errorf("push without token error = %v, want a PROXY_HOSTED_TOKEN hint", err)
	}

	err = runPush([]string{"-url", srv.URL, "cargo", "serde", "1.0.200", archive},
		hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok"}), io.Discard, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "not supported for this ecosystem") {
		t.Errorf("rejected push error = %v, want the server's message", err)
	}

	err = runPush([]string{"-url", srv.URL, "npm", "request"}, hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok"}), io.Discard, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("push with missing arguments error = %v, want usage", err)
	}
}

func TestRunPushUsesProxyURLFromEnv(t *testing.T) {
	srv, recorded := hostedTestServer(t, http.StatusCreated, `{}`)
	archive := writeArchive(t, "request-2.88.3.tgz", "tarball")

	err := runPush([]string{"npm", "request", "2.88.3", archive},
		hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok", "PROXY_URL": srv.URL}), io.Discard, srv.Client())
	if err != nil {
		t.Fatalf("runPush: %v", err)
	}
	if recorded.path != "/api/hosted/npm/request/2.88.3" {
		t.Errorf("path = %q", recorded.path)
	}
}

func TestRunList(t *testing.T) {
	srv, recorded := hostedTestServer(t, http.StatusOK,
		`{"ecosystem":"npm","name":"request","artifacts":[{"version":"2.88.3","filename":"request-2.88.3.tgz","sha256":"abc123","size":7,"pushed_at":"2026-10-13T10:00:00Z"}]}`)
	var stdout bytes.Buffer

	if err := runList([]string{"-url", srv.URL, "npm", "request"}, hostedEnv(nil), &stdout, srv.Client()); err != nil {
		t.Fatalf("runList: %v", err)
	}
	if recorded.method != http.MethodGet || recorded.path != "/api/hosted/npm/request" || recorded.authorization != "" {
		t.Errorf("request = %s %s (authorization %q)", recorded.method, recorded.path, recorded.authorization)
	}
	if out := stdout.String(); !strings.Contains(out, "2.88.3") || !strings.Contains(out, "request-2.88.3.tgz") {
		t.Errorf("output = %q", out)
	}
}

func TestRunDelete(t *testing.T) {
	srv, recorded := hostedTestServer(t, http.StatusNoContent, "")

	err := runDelete([]string{"-url", srv.URL, "npm", "request", "2.88.3"},
		hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok"}), io.Discard, srv.Client())
	if err != nil {
		t.Fatalf("runDelete: %v", err)
	}
	if recorded.method != http.MethodDelete || recorded.path != "/api/hosted/npm/request/2.88.3" || recorded.authorization != "Bearer tok" {
		t.Errorf("request = %s %s (authorization %q)", recorded.method, recorded.path, recorded.authorization)
	}

	missing, _ := hostedTestServer(t, http.StatusNotFound, `{"code":"NOT_FOUND","message":"no pushed artifacts for this version"}`)
	err = runDelete([]string{"-url", missing.URL, "npm", "request", "2.88.3"},
		hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok"}), io.Discard, missing.Client())
	if err == nil || !strings.Contains(err.Error(), "no pushed artifacts") {
		t.Errorf("delete of a missing version error = %v", err)
	}
}

func TestRunDeleteOneFile(t *testing.T) {
	srv, recorded := hostedTestServer(t, http.StatusNoContent, "")
	var stdout bytes.Buffer

	err := runDelete([]string{"-url", srv.URL, "generic", "tools", "1.2.0", "tool-linux-arm64.tar.gz"},
		hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok"}), &stdout, srv.Client())
	if err != nil {
		t.Fatalf("runDelete: %v", err)
	}
	if recorded.path != "/api/hosted/generic/tools/1.2.0/tool-linux-arm64.tar.gz" {
		t.Errorf("path = %q", recorded.path)
	}
	if out := stdout.String(); out != "deleted generic tools@1.2.0 tool-linux-arm64.tar.gz\n" {
		t.Errorf("output = %q", out)
	}
}

func TestRunDeleteOneFileOnlyForGeneric(t *testing.T) {
	srv, recorded := hostedTestServer(t, http.StatusNoContent, "")

	err := runDelete([]string{"-url", srv.URL, "npm", "@acme", "widget", "1.0.0"},
		hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok"}), io.Discard, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "only generic files") {
		t.Errorf("npm delete with a fourth argument error = %v, want a refusal", err)
	}
	if recorded.method != "" {
		t.Errorf("refused delete sent %s %s", recorded.method, recorded.path)
	}
}

func TestRunHostedDispatchesSubcommands(t *testing.T) {
	srv, recorded := hostedTestServer(t, http.StatusOK, `{"ecosystem":"npm","name":"request","artifacts":[]}`)

	if err := runHosted([]string{"list", "-url", srv.URL, "npm", "request"}, hostedEnv(nil), io.Discard, srv.Client()); err != nil {
		t.Fatalf("runHosted list: %v", err)
	}
	if recorded.method != http.MethodGet || recorded.path != "/api/hosted/npm/request" {
		t.Errorf("request = %s %s", recorded.method, recorded.path)
	}

	for _, args := range [][]string{nil, {"bogus"}} {
		err := runHosted(args, hostedEnv(nil), io.Discard, srv.Client())
		if err == nil || !strings.Contains(err.Error(), "usage: proxy hosted <push|list|delete>") {
			t.Errorf("runHosted(%q) error = %v, want the hosted usage", args, err)
		}
	}
}

func TestHostedSubcommandUsageNamesTheHostedCommand(t *testing.T) {
	err := runPush([]string{"npm"}, hostedEnv(map[string]string{"PROXY_HOSTED_TOKEN": "tok"}), io.Discard, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "usage: proxy hosted push") {
		t.Errorf("push usage error = %v, want it to name proxy hosted push", err)
	}
}
