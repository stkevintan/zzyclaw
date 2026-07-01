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

// maxFetchBody caps how many bytes a single fetch will read into memory.
const maxFetchBody = 1 << 20 // 1 MiB

// fetchTool performs an HTTP(S) request (GET/POST/PATCH/DELETE) from trusted
// host code. Network egress lives here (in audited Go), NOT inside the sandbox:
// the sandbox stays purely local, while the model can call this tool to read
// public pages or JSON APIs, or to drive REST endpoints.
//
// Access is self-maintained: a GET to a host on the operator-configured static
// allowlist is reached without prompting; any other host — and any state-changing
// method (POST/PATCH/DELETE) regardless of host — is treated as dangerous so the
// call goes through the approval/owner gate. Approving with "always" remembers
// the host+method via the grant store (see Grantable), so the allowlist
// effectively grows on demand instead of denying everything up front.
type fetchTool struct {
	policy *fetchPolicy
}

// NewFetch builds the fetch tool. allowlist holds the domains the agent may GET
// without approval (host or any subdomain); other hosts, and any state-changing
// method, require approval.
func NewFetch(allowlist []string, timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &fetchTool{policy: &fetchPolicy{
		allowlist: normalizeDomains(allowlist),
		client:    &http.Client{Timeout: timeout},
	}}
}

func (t *fetchTool) Name() string { return "fetch" }
func (t *fetchTool) Description() string {
	return "Make an HTTP(S) request (GET, POST, PATCH or DELETE) and return the response body (truncated). Optional headers and a request body may be supplied. A GET to a pre-trusted host is sent directly; other hosts and any state-changing method require one-time user approval (which can be remembered). Use it to read public web pages or JSON APIs, or to call REST endpoints."
}
func (t *fetchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"The http(s) URL to request."},"method":{"type":"string","enum":["GET","POST","PATCH","DELETE"],"description":"HTTP method (default GET)."},"body":{"type":"string","description":"Optional request body for POST/PATCH."},"headers":{"type":"object","additionalProperties":{"type":"string"},"description":"Optional request headers, e.g. {\"Content-Type\":\"application/json\"}."}},"required":["url"]}`)
}

// Dangerous is true for any state-changing method (POST/PATCH/DELETE), and for a
// GET whose host is not on the static allowlist. Such calls go through the
// approval (and owner) gate, where they can be approved once or remembered via
// GrantScope.
func (t *fetchTool) Dangerous(_ context.Context, args json.RawMessage) bool {
	host := hostFromArgs(args)
	if host == "" {
		// Malformed/unsupported URL: force the gate; Execute will surface the error.
		return true
	}
	method, err := methodFromArgs(args)
	if err != nil {
		// Unsupported method: force the gate; Execute will surface the error.
		return true
	}
	if method != http.MethodGet {
		// State-changing requests always require approval, even for trusted hosts.
		return true
	}
	return !t.policy.allowedHost(host)
}

// GrantScope lets an approval be remembered per host and method, so once (say) a
// POST to a host is approved with "always" the agent may repeat that method on
// any path of the host without re-prompting — while a different method still
// prompts.
func (t *fetchTool) GrantScope(_ context.Context, args json.RawMessage) (key, label string, ok bool) {
	host := hostFromArgs(args)
	if host == "" {
		return "", "", false
	}
	method, err := methodFromArgs(args)
	if err != nil {
		return "", "", false
	}
	return "fetch:" + method + ":" + host, method + " access to " + host, true
}

func (t *fetchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		URL     string            `json:"url"`
		Method  string            `json:"method"`
		Body    string            `json:"body"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.URL == "" {
		return "", fmt.Errorf("url must not be empty")
	}
	method, err := normalizeMethod(a.Method)
	if err != nil {
		return "", err
	}
	// Access is governed by the approval/grant gate before Execute runs; here we
	// only enforce that the URL is a well-formed http(s) request.
	if hostFromArgs(args) == "" {
		return "", fmt.Errorf("url %q must be a valid http(s) URL", a.URL)
	}
	body, err := t.policy.do(ctx, method, a.URL, a.Body, a.Headers)
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

// hostFromArgs extracts the lowercased host from a fetch arguments payload,
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

// methodFromArgs extracts the normalized HTTP method from a fetch arguments
// payload, defaulting to GET and erroring on an unsupported method.
func methodFromArgs(args json.RawMessage) (string, error) {
	var a struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	return normalizeMethod(a.Method)
}

// normalizeMethod upper-cases and validates an HTTP method, defaulting an empty
// value to GET and rejecting anything outside the supported set.
func normalizeMethod(m string) (string, error) {
	m = strings.ToUpper(strings.TrimSpace(m))
	if m == "" {
		return http.MethodGet, nil
	}
	if !fetchMethods[m] {
		return "", fmt.Errorf("unsupported method %q (allowed: GET, POST, PATCH, DELETE)", m)
	}
	return m, nil
}

// fetchMethods are the HTTP methods the fetch tool accepts.
var fetchMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodPost:   true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// do performs the HTTP request and returns up to maxFetchBody bytes of the body.
func (p *fetchPolicy) do(ctx context.Context, method, rawURL, body string, headers map[string]string) ([]byte, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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
