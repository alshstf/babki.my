package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"babki.my/babki/internal/platform/testdb"
)

// TestAHandEntryReachesTheRegistryInTheRunningProcess pins a piece of WIRING,
// which no test of a module can: that the process as cmd/babki assembles it
// hands a committed hand entry to the corporate-actions registry. The function
// that does the work existed, was tested, and was called by nothing for a month
// (#188) — so this goes through the real role, over HTTP, and asks for the
// position.
//
// Amazon's split of 2022-06-06 is recorded first; a purchase of one share dated
// 2021 is entered after it; the position must read twenty at once.
func TestAHandEntryReachesTheRegistryInTheRunningProcess(t *testing.T) {
	pool := testdb.New(t)
	addr := roleEnv(t, pool)

	ctx, cancel := context.WithCancel(context.Background())
	root := newRootCmd()
	root.SetArgs([]string{"api"})
	root.SetOut(io.Discard)
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the api role did not return after its context was cancelled")
		}
	})

	base := "http://" + addr
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 10 * time.Second, Jar: jar}

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := client.Get(base + "/api/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case runErr := <-done:
			done <- runErr
			t.Fatalf("the api role exited before it answered: %v", runErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the api role never answered on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// call posts or gets JSON and decodes the answer into out (when not nil).
	call := func(method, path, body string, want int, out any) {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, raw)
		}
		if out == nil {
			return
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
	type withID struct {
		ID string `json:"id"`
	}

	call("POST", "/api/v1/setup",
		`{"space_name":"S","username":"alex","display_name":"A","password":"secret123"}`, http.StatusCreated, nil)
	var account, paper withID
	call("POST", "/api/v1/accounts", `{"name":"Брокер","type":"brokerage","currency":"USD"}`, http.StatusCreated, &account)
	call("POST", "/api/v1/instruments",
		`{"type":"share","name":"Amazon","ticker":"AMZN","isin":"US0231351067","currency":"USD"}`, http.StatusCreated, &paper)
	call("POST", "/api/v1/instrument-events",
		`{"kind":"split","isin":"US0231351067","effective_on":"2022-06-06","ratio_from":1,"ratio_to":20,`+
			`"source_ref":"https://ir.aboutamazon.com/"}`, http.StatusCreated, nil)

	call("POST", "/api/v1/operations",
		`{"account_id":"`+account.ID+`","instrument_id":"`+paper.ID+`","type":"buy","occurred_on":"2021-05-04",`+
			`"quantity":"1","price":"3230","amount_minor":-323000,"currency":"USD"}`, http.StatusCreated, nil)

	var positions struct {
		Positions []struct {
			Quantity string `json:"quantity"`
		} `json:"positions"`
	}
	call("GET", "/api/v1/accounts/"+account.ID+"/positions", "", http.StatusOK, &positions)
	if len(positions.Positions) != 1 {
		t.Fatalf("positions = %+v, want the one paper", positions.Positions)
	}
	if got := positions.Positions[0].Quantity; got != "20" {
		t.Errorf("held = %s, want 20 — the registry's split must follow the hand entry at once, not at the next sweep", got)
	}
}
