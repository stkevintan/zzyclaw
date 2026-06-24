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

func TestHTTPGetDangerous(t *testing.T) {
	if !NewHTTPGet(nil, time.Second).Dangerous(nil) {
		t.Fatal("http_get must be dangerous (network egress needs approval)")
	}
}

func TestHTTPGetEmptyAllowlistRejects(t *testing.T) {
	tool := NewHTTPGet(nil, time.Second)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"https://example.com"}`))
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("expected allowlist error, got %v", err)
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

func TestHTTPGetBlocksDisallowedHost(t *testing.T) {
	tool := NewHTTPGet([]string{"example.com"}, time.Second)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"https://evil.com/x"}`))
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("expected allowlist rejection, got %v", err)
	}
}
