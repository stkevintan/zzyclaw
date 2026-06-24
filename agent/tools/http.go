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
// egress lives here (in audited Go), NOT inside the sandbox: the sandbox stays
// purely local, while the model can call this tool to read public pages or JSON
// APIs.
//
// Access is self-maintained: hosts on the operator-configured static allowlist
// are reached without prompting; any other host is treated as dangerous so the
// call goes through the approval/owner gate. Approving a host with "always"
// remembers it via the grant store (see Grantable), so the allowlist effectively
// grows on demand instead of denying everything up front.
type httpGetTool struct {
	policy *fetchPolicy
}

// NewHTTPGet builds the http_get tool. allowlist holds the domains the agent may
// reach without approval (host or any subdomain); other hosts require approval.
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
	return "Fetch a URL over HTTP(S) GET and return the response body (truncated). Pre-trusted hosts are fetched directly; other hosts require one-time user approval (which can be remembered). Use it to read public web pages or JSON APIs."
}
func (t *httpGetTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"The http(s) URL to fetch."}},"required":["url"]}`)
}

// Dangerous is true unless the URL's host is already on the static allowlist.
// Hosts that are not pre-trusted go through the approval (and owner) gate, where
// they can be approved once or remembered via GrantScope.
func (t *httpGetTool) Dangerous(_ context.Context, args json.RawMessage) bool {
	host := hostFromArgs(args)
	if host == "" {
		// Malformed/unsupported URL: force the gate; Execute will surface the error.
		return true
	}
	return !t.policy.allowedHost(host)
}

// GrantScope lets an approval be remembered per host, so once a host is approved
// with "always" the agent may fetch any path on it without re-prompting.
func (t *httpGetTool) GrantScope(args json.RawMessage) (key, label string, ok bool) {
	host := hostFromArgs(args)
	if host == "" {
		return "", "", false
	}
	return "http_get:" + host, "network access to " + host, true
}

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
	// Access is governed by the approval/grant gate before Execute runs; here we
	// only enforce that the URL is a well-formed http(s) request.
	if hostFromArgs(args) == "" {
		return "", fmt.Errorf("url %q must be a valid http(s) URL", a.URL)
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
// static allowlist. An empty allowlist matches nothing.
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
	return p.allowedHost(host)
}

// allowedHost reports whether a (already lowercased) host is covered by the
// static allowlist, either exactly or as a subdomain.
func (p *fetchPolicy) allowedHost(host string) bool {
	for _, d := range p.allowlist {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// hostFromArgs extracts the lowercased host from an http_get arguments payload,
// returning "" when the URL is missing, malformed or not http(s).
func hostFromArgs(args json.RawMessage) string {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return strings.ToLower(u.Hostname())
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
