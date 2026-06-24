package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchPolicyAllowed(t *testing.T) {
	p := &fetchPolicy{allowlist: normalizeDomains([]string{" Example.com ", "", "api.github.com"})}
	cases := []struct {
		url  string
		want bool
	}{
		{"https://example.com/x", true},
		{"https://www.example.com/x", true}, // subdomain allowed
		{"http://api.github.com/repos", true},
		{"https://evil.com/x", false},
		{"https://notexample.com/x", false}, // suffix must be on a dot boundary
		{"ftp://example.com/x", false},      // only http(s)
		{"file:///etc/passwd", false},
		{"not a url", false},
	}
	for _, c := range cases {
		if got := p.allowed(c.url); got != c.want {
			t.Errorf("allowed(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestFetchPolicyEmptyAllowlistDeniesAll(t *testing.T) {
	p := &fetchPolicy{allowlist: normalizeDomains(nil)}
	if p.allowed("https://example.com") {
		t.Fatal("empty allowlist must deny all")
	}
}

func TestFetchPolicyGetLimitsBody(t *testing.T) {
	big := make([]byte, maxFetchBody+1024)
	for i := range big {
		big[i] = 'a'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	p := &fetchPolicy{client: srv.Client()}
	body, err := p.get(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(body) != maxFetchBody {
		t.Fatalf("expected body capped at %d, got %d", maxFetchBody, len(body))
	}
}

func TestHTTPGetDangerousByHost(t *testing.T) {
	tool := NewHTTPGet([]string{"example.com"}, time.Second)
	// Allowlisted host: pre-trusted, no approval prompt.
	if tool.Dangerous(context.Background(), json.RawMessage(`{"url":"https://example.com/x"}`)) {
		t.Fatal("allowlisted host must not be dangerous")
	}
	// Any other host requires approval.
	if !tool.Dangerous(context.Background(), json.RawMessage(`{"url":"https://evil.com/x"}`)) {
		t.Fatal("non-allowlisted host must be dangerous (needs approval)")
	}
	// Malformed/empty URL forces the gate too.
	if !tool.Dangerous(context.Background(), nil) {
		t.Fatal("malformed args must be treated as dangerous")
	}
}

func TestHTTPGetGrantScope(t *testing.T) {
	g, ok := NewHTTPGet(nil, time.Second).(Grantable)
	if !ok {
		t.Fatal("http_get must implement Grantable")
	}
	key, label, ok := g.GrantScope(json.RawMessage(`{"url":"https://api.example.com/v1"}`))
	if !ok || key != "http_get:api.example.com" {
		t.Fatalf("GrantScope = (%q,%q,%v), want key http_get:api.example.com", key, label, ok)
	}
	if _, _, ok := g.GrantScope(json.RawMessage(`{"url":"not a url"}`)); ok {
		t.Fatal("malformed URL must not be grantable")
	}
}

func TestHTTPGetFetchesAllowlisted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()

	// Allow the httptest server's host (127.0.0.1) so the request is permitted.
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.IndexByte(host, ':')]
	tool := NewHTTPGet([]string{host}, 5*time.Second)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "pong" {
		t.Fatalf("expected body %q, got %q", "pong", out)
	}
}

func TestHTTPGetRejectsMalformedURL(t *testing.T) {
	tool := NewHTTPGet(nil, time.Second)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"ftp://example.com/x"}`)); err == nil {
		t.Fatal("expected error for non-http(s) URL")
	}
}
