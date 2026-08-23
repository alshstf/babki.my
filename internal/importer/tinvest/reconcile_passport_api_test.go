package tinvest

import (
	"encoding/json"
	"testing"
)

// TestMismatchesAPIKeepsOldRowsPassportless: the differences of runs recorded
// before the passport fields existed carry no such jsonb keys, and they must
// come back as an explicit «no passport» — every nullable null — rather than
// as a decode error or a passport of empty strings. The jsonb here is
// verbatim what reconcileColumns wrote in those days.
func TestMismatchesAPIKeepsOldRowsPassportless(t *testing.T) {
	raw := json.RawMessage(
		`[{"kind":"unknown_security","label":"TECH2","broker":"60795","journal":"0"}]`)

	out, err := mismatchesAPI(raw)
	if err != nil {
		t.Fatalf("mismatchesAPI: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("rows = %d, want 1", len(out))
	}
	m := out[0]
	if !m.BrokerIsin.IsNull() || !m.BrokerName.IsNull() || !m.BrokerCurrency.IsNull() || !m.BrokerType.IsNull() {
		t.Errorf("passport of an old row = %v/%v/%v/%v, want four explicit nulls",
			m.BrokerIsin, m.BrokerName, m.BrokerCurrency, m.BrokerType)
	}
}

// TestMismatchesAPIRoundTripsThePassport is the other half: a row recorded
// WITH a passport publishes it field for field, the type in this catalog's own
// vocabulary.
func TestMismatchesAPIRoundTripsThePassport(t *testing.T) {
	raw := json.RawMessage(
		`[{"kind":"unknown_security","label":"TECH2","broker":"60795","journal":"0",` +
			`"broker_isin":"RU000A1071G8","broker_name":"Заблокированные активы",` +
			`"broker_currency":"RUB","broker_type":"etf"}]`)

	out, err := mismatchesAPI(raw)
	if err != nil {
		t.Fatalf("mismatchesAPI: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("rows = %d, want 1", len(out))
	}
	m := out[0]
	isin, err := m.BrokerIsin.Get()
	if err != nil || isin != "RU000A1071G8" {
		t.Errorf("broker_isin = %v (%v), want RU000A1071G8", m.BrokerIsin, err)
	}
	typ, err := m.BrokerType.Get()
	if err != nil || string(typ) != "etf" {
		t.Errorf("broker_type = %v (%v), want etf", m.BrokerType, err)
	}
	cur, err := m.BrokerCurrency.Get()
	if err != nil || cur != "RUB" {
		t.Errorf("broker_currency = %v (%v), want RUB", m.BrokerCurrency, err)
	}
}
