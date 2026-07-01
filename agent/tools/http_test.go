package tools

import (
	"context"
	"encoding/json"
	"io"
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
	body, err := p.do(context.Background(), http.MethodGet, srv.URL, "", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if len(body) != maxFetchBody {
		t.Fatalf("expected body capped at %d, got %d", maxFetchBody, len(body))
	}
}

func TestFetchDangerousByHost(t *testing.T) {
	tool := NewFetch([]string{"example.com"}, time.Second)
	// Allowlisted host with a GET: pre-trusted, no approval prompt.
	if tool.Dangerous(context.Background(), json.RawMessage(`{"url":"https://example.com/x"}`)) {
		t.Fatal("allowlisted host GET must not be dangerous")
	}
	// Any other host requires approval.
	if !tool.Dangerous(context.Background(), json.RawMessage(`{"url":"https://evil.com/x"}`)) {
		t.Fatal("non-allowlisted host must be dangerous (needs approval)")
	}
	// State-changing methods require approval even on an allowlisted host.
	for _, m := range []string{"POST", "PATCH", "DELETE"} {
		args := json.RawMessage(`{"url":"https://example.com/x","method":"` + m + `"}`)
		if !tool.Dangerous(context.Background(), args) {
			t.Fatalf("%s to allowlisted host must be dangerous", m)
		}
	}
	// Unsupported method forces the gate.
	if !tool.Dangerous(context.Background(), json.RawMessage(`{"url":"https://example.com/x","method":"PUT"}`)) {
		t.Fatal("unsupported method must be treated as dangerous")
	}
	// Malformed/empty URL forces the gate too.
	if !tool.Dangerous(context.Background(), nil) {
		t.Fatal("malformed args must be treated as dangerous")
	}
}

func TestFetchGrantScope(t *testing.T) {
	g, ok := NewFetch(nil, time.Second).(Grantable)
	if !ok {
		t.Fatal("fetch must implement Grantable")
	}
	key, label, ok := g.GrantScope(context.Background(), json.RawMessage(`{"url":"https://api.example.com/v1"}`))
	if !ok || key != "fetch:GET:api.example.com" {
		t.Fatalf("GrantScope = (%q,%q,%v), want key fetch:GET:api.example.com", key, label, ok)
	}
	// The method is part of the scope, so a POST is remembered separately.
	key, _, ok = g.GrantScope(context.Background(), json.RawMessage(`{"url":"https://api.example.com/v1","method":"post"}`))
	if !ok || key != "fetch:POST:api.example.com" {
		t.Fatalf("GrantScope POST key = %q, want fetch:POST:api.example.com", key)
	}
	if _, _, ok := g.GrantScope(context.Background(), json.RawMessage(`{"url":"not a url"}`)); ok {
		t.Fatal("malformed URL must not be grantable")
	}
}

func TestFetchFetchesAllowlisted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()

	// Allow the httptest server's host (127.0.0.1) so the request is permitted.
	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.IndexByte(host, ':')]
	tool := NewFetch([]string{host}, 5*time.Second)

	out, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "pong" {
		t.Fatalf("expected body %q, got %q", "pong", out)
	}
}

func TestFetchPostSendsBodyAndHeaders(t *testing.T) {
	var gotMethod, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	host = host[:strings.IndexByte(host, ':')]
	tool := NewFetch([]string{host}, 5*time.Second)

	args := json.RawMessage(`{"url":"` + srv.URL + `","method":"POST","body":"{\"a\":1}","headers":{"Content-Type":"application/json"}}`)
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "ok" {
		t.Fatalf("expected body %q, got %q", "ok", out)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("expected POST, got %s", gotMethod)
	}
	if gotType != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %q", gotType)
	}
	if gotBody != `{"a":1}` {
		t.Fatalf("expected body %q, got %q", `{"a":1}`, gotBody)
	}
}

func TestFetchRejectsUnsupportedMethod(t *testing.T) {
	tool := NewFetch([]string{"example.com"}, time.Second)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"https://example.com/x","method":"PUT"}`)); err == nil {
		t.Fatal("expected error for unsupported method")
	}
}

func TestFetchRejectsMalformedURL(t *testing.T) {
	tool := NewFetch(nil, time.Second)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"url":"ftp://example.com/x"}`)); err == nil {
		t.Fatal("expected error for non-http(s) URL")
	}
}
