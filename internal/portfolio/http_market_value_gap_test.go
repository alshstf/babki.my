package portfolio_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/testdb"
)

// market_value_gap names why a valuation is missing (#78): no quote, a type
// without a valuation model (crypto, currency, metal, custom — a quote may be
// present), or a bond without a face value. Each test asserts the specific
// value and the null valuation beside it.

// quotedAPI wires a RUB space with the given quotes and an unseeded converter;
// every fixture is RUB, so nothing converts.
func quotedAPI(t *testing.T, quotes quoteStoreLike) (string, *http.Client) {
	t.Helper()
	pool := testdb.New(t)
	return setupAPI(t, pool, quotes, marketdata.NewConverter(marketdata.NewStore(pool)))
}

// mustUUID parses an id handed back by the API, which is where the fixtures
// below get the key their fake quote store is filed under.
func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse id %q: %v", s, err)
	}
	return id
}

// no_quote: a priced type with a complete catalog row and no price.
func TestPositionMarketValueGapNamesTheMissingQuote(t *testing.T) {
	url, c := quotedAPI(t, &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, share.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.MarketValueMinor != nil {
		t.Fatalf("market_value_minor = %d, want null: there is no quote to value this position from", *p.MarketValueMinor)
	}
	if p.MarketValueGap == nil || *p.MarketValueGap != "no_quote" {
		t.Fatalf("market_value_gap = %s, want no_quote: a share with a complete catalog row is missing nothing but the price",
			gapText(p.MarketValueGap))
	}
}

// type_not_priced: a crypto position with a fresh quote that will never value
// it.
func TestPositionMarketValueGapNamesAnUnpricedTypeThatHasAQuote(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := quotedAPI(t, quotes)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	coin := createInstrument(t, c, url, `{"type":"crypto","name":"Биткоин","ticker":"BTC","currency":"RUB"}`)
	quotes.byInstrument[mustUUID(t, coin.ID)] = marketdata.Quote{
		InstrumentID: mustUUID(t, coin.ID), On: mustDate(t, "2026-07-22"),
		Price: decimal.RequireFromString("5000000"), Currency: "RUB", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"2","price":"4000000",
		"amount_minor":-800000000,"currency":"RUB"}`, acc.ID, coin.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.MarketValueMinor != nil {
		t.Fatalf("market_value_minor = %d, want null: this program has no valuation model for crypto", *p.MarketValueMinor)
	}
	if p.MarketValueGap == nil || *p.MarketValueGap != "type_not_priced" {
		t.Fatalf("market_value_gap = %s, want type_not_priced: the quote for this instrument exists and is not what is missing",
			gapText(p.MarketValueGap))
	}
	// No price line either: the cell is not derived from it.
	if p.Price != nil || p.PriceOn != nil {
		t.Errorf("price/price_on = %v/%v, want both null: no valuation was struck from this quote", p.Price, p.PriceOn)
	}
}

// With no quote and an unpriced type, the type is named: a quote would close
// nothing.
func TestPositionMarketValueGapPrefersTheUnpricedTypeToTheMissingQuote(t *testing.T) {
	url, c := quotedAPI(t, &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	gold := createInstrument(t, c, url, `{"type":"metal","name":"Золото","ticker":"XAU","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"3","price":"800000",
		"amount_minor":-240000000,"currency":"RUB"}`, acc.ID, gold.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.MarketValueGap == nil || *p.MarketValueGap != "type_not_priced" {
		t.Fatalf("market_value_gap = %s, want type_not_priced: a quote for a metal would not produce a valuation, so the missing quote is not the news",
			gapText(p.MarketValueGap))
	}
}

// no_face_value: a bond without a face value has nothing to apply its
// percentage to.
func TestPositionMarketValueGapNamesTheMissingFaceValue(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := quotedAPI(t, quotes)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	// Both halves of the face pair omitted, which is the only shape the
	// catalog admits: migration 0012's CHECK keeps them both-or-neither.
	bond := createInstrument(t, c, url, `{"type":"bond","name":"Облигация","ticker":"BOND1","currency":"RUB"}`)
	quotes.byInstrument[mustUUID(t, bond.ID)] = marketdata.Quote{
		InstrumentID: mustUUID(t, bond.ID), On: mustDate(t, "2026-07-21"),
		Price: decimal.RequireFromString("95.20"), Currency: "RUB", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"100","price":"950",
		"amount_minor":-9500000,"currency":"RUB"}`, acc.ID, bond.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.MarketValueMinor != nil {
		t.Fatalf("market_value_minor = %d, want null: 95.20%% of an unrecorded face value is not a figure", *p.MarketValueMinor)
	}
	if p.MarketValueGap == nil || *p.MarketValueGap != "no_face_value" {
		t.Fatalf("market_value_gap = %s, want no_face_value: the quote is present and is not what stops this valuation",
			gapText(p.MarketValueGap))
	}
}

// With no quote and no face value, the face value is named.
func TestPositionMarketValueGapPrefersTheMissingFaceValueToTheMissingQuote(t *testing.T) {
	url, c := quotedAPI(t, &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}})

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	bond := createInstrument(t, c, url, `{"type":"bond","name":"Облигация","ticker":"BOND2","currency":"RUB"}`)
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"100","price":"950",
		"amount_minor":-9500000,"currency":"RUB"}`, acc.ID, bond.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.MarketValueGap == nil || *p.MarketValueGap != "no_face_value" {
		t.Fatalf("market_value_gap = %s, want no_face_value: a percentage quote for a bond with no face value would still value nothing",
			gapText(p.MarketValueGap))
	}
}

// A struck valuation has a null gap.
func TestPositionMarketValueGapNullWhenTheValuationIsStruck(t *testing.T) {
	quotes := &fakeQuoteStore{byInstrument: map[uuid.UUID]marketdata.Quote{}}
	url, c := quotedAPI(t, quotes)

	acc := createAccount(t, c, url, `{"name":"Брокер","type":"brokerage","currency":"RUB"}`)
	share := createInstrument(t, c, url, `{"type":"share","name":"Акция","ticker":"ACME","currency":"RUB"}`)
	quotes.byInstrument[mustUUID(t, share.ID)] = marketdata.Quote{
		InstrumentID: mustUUID(t, share.ID), On: mustDate(t, "2026-07-20"),
		Price: decimal.RequireFromString("120"), Currency: "RUB", Source: "test",
	}
	createOperation(t, c, url, fmt.Sprintf(`{"account_id":%q,"instrument_id":%q,"type":"buy",
		"occurred_on":"2026-07-01","quantity":"10","price":"100",
		"amount_minor":-100000,"currency":"RUB"}`, acc.ID, share.ID))

	p := onlyPosition(t, c, url, acc.ID)
	if p.MarketValueMinor == nil || *p.MarketValueMinor != 120000 {
		t.Fatalf("market_value_minor = %v, want 120000 (10 × 120,00 ₽)", p.MarketValueMinor)
	}
	if p.MarketValueGap != nil {
		t.Fatalf("market_value_gap = %s, want null: the valuation was struck and is in the position's own currency",
			gapText(p.MarketValueGap))
	}
}
