package tinvest_test

// This file is package tinvest_test (external, exported-API only)
// deliberately: SandboxService's mutating calls (open an account, fund it,
// place an order) are test scaffolding for exercising the real client's
// read methods against a live sandbox account, not something the importer
// itself ever needs — the importer only reads a broker's history, it never
// trades. Those calls therefore live only in this test file, built as raw
// requests, rather than being added to Client's public surface.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/importer/tinvest"
)

// sberFIGI is Sberbank ordinary shares' FIGI — a liquid, always-tradable
// instrument used here only so PostSandboxOrder has something to buy one
// unit of. It is a well-known identifier reused across T-Invest API
// examples; this test has not independently re-verified it via a live
// instrument lookup (InstrumentByUID takes a UID, not a FIGI, and this test
// deliberately does not add a FIGI-lookup method to the client just to
// self-check a constant used only here).
const sberFIGI = "BBG004730N88"

// sandboxRPC performs one raw POST call against a SandboxService method,
// following the same wire convention Client itself uses (POST
// {base}/tinkoff.public.invest.api.contract.v1.<Service>/<Method>, Bearer
// auth, JSON body in and out) — duplicated here rather than reusing Client
// internals, since this package-external test file has no access to them
// and, per the task brief, should not: these calls are not part of the
// client's public API.
func sandboxRPC(ctx context.Context, hc *http.Client, token, method string, reqBody, respBody any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	url := tinvest.SandboxBaseURL + "/tinkoff.public.invest.api.contract.v1." + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body := new(bytes.Buffer)
	if _, err := body.ReadFrom(resp.Body); err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d: %s", method, resp.StatusCode, strings.TrimSpace(body.String()))
	}
	if respBody == nil {
		return nil
	}
	if err := json.Unmarshal(body.Bytes(), respBody); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	return nil
}

// TestSandboxLiveRoundTrip exercises the real client against T-Invest's
// live sandbox gateway. It is gated on BABKI_TINVEST_SANDBOX_TOKEN, read
// only from the environment and never written anywhere — see the task
// brief for why the token must not reach the repository in any form.
//
// Run it explicitly with:
//
//	BABKI_TINVEST_SANDBOX_TOKEN='...' go test ./internal/importer/tinvest/ -run Sandbox -v
//
// What this proves, live: the embedded certificate and TLS transport
// actually complete a handshake against sandbox-invest-public-api.tinkoff.ru
// (not just against an httptest.Server using the test binary's own trust),
// that GetAccounts and OperationsAll — the client's own public methods —
// work against the sandbox host, and that OperationsAll's cursor walk
// terminates on an empty history rather than looping.
//
// What it does not and cannot prove: the sandbox only ever produces
// BUY/SELL operations with a flat 0.05% commission (see the task brief) —
// no dividends, no taxes, no BROKER_FEE — so it says nothing about how this
// client (or a later task) handles those. That gap is intentional, not an
// oversight: the sandbox itself cannot exercise it.
func TestSandboxLiveRoundTrip(t *testing.T) {
	token := os.Getenv("BABKI_TINVEST_SANDBOX_TOKEN")
	if token == "" {
		t.Skip("BABKI_TINVEST_SANDBOX_TOKEN not set; skipping the live sandbox test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	hc, err := tinvest.NewHTTPClient(30 * time.Second)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	client := tinvest.NewClient(hc, tinvest.SandboxBaseURL, token, nil)

	// A fresh account per run, rather than reusing whatever already exists
	// on this token: the sandbox docs themselves say accounts are not
	// guaranteed to persist, and a fresh account also guarantees the empty-
	// history check below actually starts from zero operations.
	var openResp struct {
		AccountID string `json:"accountId"`
	}
	if err := sandboxRPC(ctx, hc, token, "SandboxService/OpenSandboxAccount", struct{}{}, &openResp); err != nil {
		t.Fatalf("OpenSandboxAccount: %v", err)
	}
	accountID := openResp.AccountID
	if accountID == "" {
		t.Fatalf("OpenSandboxAccount returned an empty accountId")
	}
	t.Logf("opened sandbox account %s", accountID)
	t.Cleanup(func() {
		// Best effort: a leaked sandbox account costs nothing but tidiness,
		// so a failed cleanup call must not fail the test itself.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := sandboxRPC(closeCtx, hc, token, "SandboxService/CloseSandboxAccount",
			map[string]string{"accountId": accountID}, nil); err != nil {
			t.Logf("cleanup: CloseSandboxAccount(%s): %v", accountID, err)
		}
	})

	t.Run("GetAccounts sees the account over the live sandbox transport", func(t *testing.T) {
		accounts, err := client.GetAccounts(ctx)
		if err != nil {
			t.Fatalf("GetAccounts: %v", err)
		}
		found := false
		for _, a := range accounts {
			if a.ID == accountID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("GetAccounts returned %d accounts not including the freshly opened %s", len(accounts), accountID)
		}
	})

	t.Run("OperationsAll terminates on empty history", func(t *testing.T) {
		items, err := client.OperationsAll(ctx, accountID, time.Time{})
		if err != nil {
			t.Fatalf("OperationsAll: %v", err)
		}
		if len(items) != 0 {
			t.Errorf("OperationsAll on a brand-new, unfunded account returned %d items, want 0", len(items))
		}
	})

	// Fund the account and buy one share, so there is a BUY operation for
	// OperationsAll to find.
	payIn := map[string]any{
		"accountId": accountID,
		"amount":    map[string]any{"currency": "rub", "units": "100000", "nano": 0},
	}
	if err := sandboxRPC(ctx, hc, token, "SandboxService/SandboxPayIn", payIn, nil); err != nil {
		t.Fatalf("SandboxPayIn: %v", err)
	}

	order := map[string]any{
		"accountId":    accountID,
		"instrumentId": sberFIGI,
		"quantity":     "1",
		"direction":    "ORDER_DIRECTION_BUY",
		"orderType":    "ORDER_TYPE_MARKET",
		"orderId":      uuid.NewString(),
	}
	if err := sandboxRPC(ctx, hc, token, "SandboxService/PostSandboxOrder", order, nil); err != nil {
		// A market order can be legitimately refused for a reason outside
		// this client's control — observed live on 2026-08-04 at ~01:30
		// Moscow time, outside MOEX's trading session: both a SBER limit
		// order and a USDRUB market order came back "Instrument is not
		// available for trading" (broker error 30079). That is the exchange
		// being closed, not a defect in this request, so it is reported as
		// a skip (with the broker's own error surfaced) rather than a
		// failure — a real trading-hours outage should not read the same as
		// a broken client.
		t.Skipf("PostSandboxOrder: %v (if this is broker error 30079 / \"Instrument is not available for "+
			"trading\", it most likely means the exchange session is closed right now — rerun during MOEX "+
			"trading hours to exercise the BUY-visibility check below)", err)
	}

	t.Run("OperationsAll sees the BUY", func(t *testing.T) {
		// Sandbox order fills are not guaranteed synchronous with
		// PostSandboxOrder's response, so poll briefly instead of checking
		// exactly once and risking a false failure on a slow fill.
		deadline := time.Now().Add(30 * time.Second)
		var items []tinvest.OperationItem
		for {
			var err error
			items, err = client.OperationsAll(ctx, accountID, time.Time{})
			if err != nil {
				t.Fatalf("OperationsAll: %v", err)
			}
			if hasOperationType(items, "OPERATION_TYPE_BUY") || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Second)
		}
		if !hasOperationType(items, "OPERATION_TYPE_BUY") {
			t.Fatalf("OperationsAll after a market BUY order returned %d items, none of them OPERATION_TYPE_BUY: %+v",
				len(items), items)
		}
	})
}

func hasOperationType(items []tinvest.OperationItem, opType string) bool {
	for _, it := range items {
		if it.Type == opType {
			return true
		}
	}
	return false
}
