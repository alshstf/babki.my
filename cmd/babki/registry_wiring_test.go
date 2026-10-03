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

// runAPI starts the api role as cmd/babki assembles it, waits until it
// answers, and returns a caller that posts or gets JSON and decodes the answer
// into out (when not nil).
func runAPI(t *testing.T) func(method, path, body string, want int, out any) {
	t.Helper()
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

	return func(method, path, body string, want int, out any) {
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
}

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
	call := runAPI(t)
	type withID struct {
		ID string `json:"id"`
	}

	call("POST", "/api/v1/setup",
		`{"space_name":"S","username":"alex","display_name":"A","password":"secret123","setup_code":"`+testSetupCode+`"}`, http.StatusCreated, nil)
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

// TestTheTotalReadsABrokerageAccountFromItsJournal pins the other piece of
// wiring the account module cannot test alone: that the process hands it the
// portfolio engine, so the family total counts a brokerage account by its
// operations (the owner's ruling on Р-2) rather than by a balance typed in.
func TestTheTotalReadsABrokerageAccountFromItsJournal(t *testing.T) {
	call := runAPI(t)
	call("POST", "/api/v1/setup",
		`{"space_name":"S","username":"alex","display_name":"A","password":"secret123","setup_code":"`+testSetupCode+`"}`, http.StatusCreated, nil)
	var account struct {
		ID string `json:"id"`
	}
	call("POST", "/api/v1/accounts", `{"name":"Брокер","type":"brokerage","currency":"RUB"}`, http.StatusCreated, &account)
	call("POST", "/api/v1/operations",
		`{"account_id":"`+account.ID+`","type":"deposit","occurred_on":"2026-07-01","amount_minor":10000000,"currency":"RUB"}`,
		http.StatusCreated, nil)
	call("PUT", "/api/v1/accounts/"+account.ID+"/balance",
		`{"as_of":"`+time.Now().UTC().Format("2006-01-02")+`","amount_minor":9900000}`, http.StatusOK, nil)

	var summary struct {
		TotalInBaseMinor int64 `json:"total_in_base_minor"`
		Journal          struct {
			Accounts int `json:"accounts"`
		} `json:"journal"`
	}
	call("GET", "/api/v1/summary", "", http.StatusOK, &summary)
	if summary.TotalInBaseMinor != 10_000_000 || summary.Journal.Accounts != 1 {
		t.Errorf("summary = %+v, want the 100 000 ₽ the journal holds, not the 99 000 typed in", summary)
	}
	var rows []struct {
		CountedBy string `json:"counted_by"`
		Journal   struct {
			Reconciliation struct {
				Status string `json:"status"`
			} `json:"reconciliation"`
		} `json:"journal"`
	}
	call("GET", "/api/v1/accounts", "", http.StatusOK, &rows)
	if len(rows) != 1 || rows[0].CountedBy != "journal" || rows[0].Journal.Reconciliation.Status != "close" {
		t.Errorf("accounts = %+v, want the one account counted by its journal, close to its balance", rows)
	}
}
