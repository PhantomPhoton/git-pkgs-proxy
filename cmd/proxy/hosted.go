package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	defaultProxyURL     = "http://localhost:8080"
	hostedTokenEnv      = "PROXY_HOSTED_TOKEN"
	hostedClientTimeout = 10 * time.Minute

	pushArgCount   = 4 // ecosystem, name, version, file
	listArgCount   = 2 // ecosystem, name
	deleteArgCount = 3 // ecosystem, name, version
	tablePadding   = 2
)

// hostedArtifact mirrors the JSON the /api/hosted endpoints return.
type hostedArtifact struct {
	Version  string    `json:"version"`
	Filename string    `json:"filename"`
	SHA256   string    `json:"sha256"`
	Size     int64     `json:"size"`
	PushedAt time.Time `json:"pushed_at"`
}

type hostedPushResponse struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	hostedArtifact
}

type hostedListResponse struct {
	Ecosystem string           `json:"ecosystem"`
	Name      string           `json:"name"`
	Artifacts []hostedArtifact `json:"artifacts"`
}

type hostedCommand struct {
	flags    *flag.FlagSet
	proxyURL *string
	usage    string
}

func newHostedCommand(name, usage, summary string, getenv func(string) string) hostedCommand {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	proxyURL := getenv("PROXY_URL")
	if proxyURL == "" {
		proxyURL = defaultProxyURL
	}
	cmd := hostedCommand{flags: fs, usage: usage}
	cmd.proxyURL = fs.String("url", proxyURL, "Proxy URL (env PROXY_URL)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "git-pkgs proxy - %s\n\nUsage: %s\n\nFlags:\n", summary, usage)
		fs.SetOutput(os.Stderr)
		fs.PrintDefaults()
	}
	return cmd
}

// parse parses flags and checks for exactly want positional arguments.
func (c hostedCommand) parse(args []string, want int) ([]string, error) {
	if err := c.flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			c.flags.Usage()
		}
		return nil, fmt.Errorf("%w\nusage: %s", err, c.usage)
	}
	if c.flags.NArg() != want {
		return nil, fmt.Errorf("usage: %s", c.usage)
	}
	return c.flags.Args(), nil
}

func (c hostedCommand) endpoint(ecosystem, name string, version ...string) string {
	segments := []string{url.PathEscape(ecosystem)}
	for segment := range strings.SplitSeq(name, "/") {
		segments = append(segments, url.PathEscape(segment))
	}
	for _, v := range version {
		segments = append(segments, url.PathEscape(v))
	}
	return strings.TrimSuffix(*c.proxyURL, "/") + "/api/hosted/" + strings.Join(segments, "/")
}

func hostedToken(getenv func(string) string) (string, error) {
	token := getenv(hostedTokenEnv)
	if token == "" {
		return "", fmt.Errorf("%s must be set to the proxy's hosted.token", hostedTokenEnv)
	}
	return token, nil
}

func runPush(args []string, getenv func(string) string, stdout io.Writer, client *http.Client) error {
	cmd := newHostedCommand("push", "proxy push [-url URL] <ecosystem> <name> <version> <file>",
		"Push a package archive to the proxy", getenv)
	positional, err := cmd.parse(args, pushArgCount)
	if err != nil {
		return err
	}
	token, err := hostedToken(getenv)
	if err != nil {
		return err
	}
	ecosystem, name, version, path := positional[0], positional[1], positional[2], positional[3]

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	req, err := http.NewRequest(http.MethodPut, cmd.endpoint(ecosystem, name, version), file)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")

	var pushed hostedPushResponse
	if err := doHosted(client, req, http.StatusCreated, &pushed); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "pushed %s %s@%s as %s (%d bytes, sha256 %s)\n",
		pushed.Ecosystem, pushed.Name, pushed.Version, pushed.Filename, pushed.Size, pushed.SHA256)
	return err
}

func runList(args []string, getenv func(string) string, stdout io.Writer, client *http.Client) error {
	cmd := newHostedCommand("list", "proxy list [-url URL] <ecosystem> <name>", "List pushed package archives", getenv)
	positional, err := cmd.parse(args, listArgCount)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, cmd.endpoint(positional[0], positional[1]), nil)
	if err != nil {
		return err
	}

	var listed hostedListResponse
	if err := doHosted(client, req, http.StatusOK, &listed); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, tablePadding, ' ', 0)
	_, _ = fmt.Fprintln(tw, "VERSION\tFILENAME\tSIZE\tSHA256\tPUSHED")
	for _, a := range listed.Artifacts {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", a.Version, a.Filename, a.Size, a.SHA256, a.PushedAt.Format(time.RFC3339))
	}
	return tw.Flush()
}

func runDelete(args []string, getenv func(string) string, stdout io.Writer, client *http.Client) error {
	cmd := newHostedCommand("delete", "proxy delete [-url URL] <ecosystem> <name> <version>", "Delete a pushed version", getenv)
	positional, err := cmd.parse(args, deleteArgCount)
	if err != nil {
		return err
	}
	token, err := hostedToken(getenv)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodDelete, cmd.endpoint(positional[0], positional[1], positional[2]), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	if err := doHosted(client, req, http.StatusNoContent, nil); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "deleted %s %s@%s\n", positional[0], positional[1], positional[2])
	return err
}

// doHosted sends req and decodes a successful response into out. Any other
// status is returned as an error carrying the server's message.
func doHosted(client *http.Client, req *http.Request, wantStatus int, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != wantStatus {
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.NewDecoder(resp.Body).Decode(&apiErr) == nil && apiErr.Message != "" {
			return fmt.Errorf("%s %s: %s (%s)", req.Method, req.URL.Path, apiErr.Message, resp.Status)
		}
		return fmt.Errorf("%s %s: %s", req.Method, req.URL.Path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// runHostedCommand runs one of the push, list and delete commands against the
// proxy's /api/hosted endpoints and exits non-zero on failure.
func runHostedCommand(run func([]string, func(string) string, io.Writer, *http.Client) error) {
	client := &http.Client{Timeout: hostedClientTimeout}
	if err := run(os.Args[1:], os.Getenv, os.Stdout, client); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
