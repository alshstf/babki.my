// Package apitest is what the modules' HTTP tests share: a running server
// with an owner signed in, and the request helpers (plan item 2.7). Which
// handlers a test mounts stays the test's own.
package apitest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

// Owner is the first-run setup every test's server is given.
const Owner = `{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`

// Serve runs h for the test and signs the owner in through first-run setup;
// the client keeps the session.
func Serve(t *testing.T, h http.Handler) (string, *http.Client) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Post(ts.URL+"/api/v1/setup", "application/json", strings.NewReader(Owner))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	return ts.URL, client
}

// Do sends a request with a JSON body, or none.
func Do(t *testing.T, c *http.Client, method, url, body string) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// Decode reads a JSON answer into dst and closes it.
func Decode(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode %s: %v", resp.Request.URL, err)
	}
}
