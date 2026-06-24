package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxFetchBody caps how many bytes a single http_get will read into memory.
const maxFetchBody = 1 << 20 // 1 MiB

// httpGetTool fetches a URL over HTTP(S) GET from trusted host code. Network
// egress lives here (in audited Go), NOT inside the WASI sandbox: the sandbox
// stays purely local, while the model can call this tool to read public pages or
// JSON APIs. Requests are confined to an operator-configured domain allowlist and
// the tool is marked dangerous so each call goes through the approval/owner gate.
type httpGetTool struct {
	policy *fetchPolicy
}

// NewHTTPGet builds the http_get tool. allowlist holds the domains the agent may
// reach (host or any subdomain); an empty allowlist denies all network.
func NewHTTPGet(allowlist []string, timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &httpGetTool{policy: &fetchPolicy{
		allowlist: normalizeDomains(allowlist),
		client:    &http.Client{Timeout: timeout},
	}}
}

func (t *httpGetTool) Name() string { return "http_get" }
func (t *httpGetTool) Description() string {
	return "Fetch a URL over HTTP(S) GET and return the response body (truncated). Limited to operator-allowlisted domains and requires approval. Use it to read public web pages or JSON APIs."
}
func (t *httpGetTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"The http(s) URL to fetch."}},"required":["url"]}`)
}

// Dangerous is always true: outbound network access goes through the approval
// (and owner) gate even though it is restricted to the allowlist.
func (t *httpGetTool) Dangerous(json.RawMessage) bool { return true }

func (t *httpGetTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.URL == "" {
		return "", fmt.Errorf("url must not be empty")
	}
	if !t.policy.allowed(a.URL) {
		if len(t.policy.allowlist) == 0 {
			return "", fmt.Errorf("network is disabled: no domains are allowlisted (set agent.network_allowlist)")
		}
		return "", fmt.Errorf("url %q is not in the allowlist", a.URL)
	}
	body, err := t.policy.get(ctx, a.URL)
	if err != nil {
		return "", fmt.Errorf("fetch failed: %w", err)
	}
	return truncateOutput(string(body)), nil
}

// fetchPolicy mediates and constrains outbound HTTP access.
type fetchPolicy struct {
	allowlist []string // lowercased domains; host or *.host is allowed
	client    *http.Client
}

// allowed reports whether rawURL is an http(s) URL whose host is covered by the
// allowlist. An empty allowlist denies everything.
func (p *fetchPolicy) allowed(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	for _, d := range p.allowlist {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// get performs an HTTP GET and returns up to maxFetchBody bytes of the body.
func (p *fetchPolicy) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxFetchBody))
}

// normalizeDomains lowercases and trims allowlist entries, dropping blanks.
func normalizeDomains(in []string) []string {
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}
