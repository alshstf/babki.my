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

// This file is issue #78's server half. A position's valuation cell can be
// empty for three different reasons, and the payload used to say only that it
// was: market_value_minor went null and the screen put «Нет котировки» over
// every one of them. That sentence is false about an instrument whose TYPE this
// program has no valuation model for — crypto, currency, metal, custom — where
// a quote may exist and be perfectly good, and false again about a bond whose
// face value nobody recorded, whose quote is a percentage with nothing to take
// a percentage of. Both send the reader off to wait for data that is not
// missing. market_value_gap names the cause instead.
//
// Every test here asserts the SPECIFIC value, never merely that some gap is
// set — the same rule http_position_in_base_gap_test.go states and for the same
// reason: a cause that is published but wrong is worse than the vague sentence
// it replaces.
//
// Each test also asserts that market_value_minor is null beside it. The two are
// one claim in the contract ("the first three say there is no valuation at
// all"), and a gap naming an absent figure that is in fact present would be a
// dash-caption sitting over a number.

// quotedAPI wires an RUB-based space (setupAPI's default) with the given quote
// store and a real, unseeded converter. Nothing here needs an fx rate: every
// fixture below holds a RUB position in a RUB-based space, so no conversion is
// ever attempted and market_value_gap is the only thing under test.
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

// TestPositionMarketValueGapNamesTheMissingQuote is the value that keeps its
// old meaning, and the only one of the three an arriving quote closes: the
// instrument is a share, which this program prices, and its catalog row holds
// everything the valuation needs. Nothing but the price is missing.
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

// TestPositionMarketValueGapNamesAnUnpricedTypeThatHasAQuote is #78 itself.
// The quote is there, it is fresh, and it is never going to become a
// valuation, because this program computes none for a crypto position. A
// payload that answered `no_quote` here would not merely be vague — it would
// name a thing that is present, and point the reader at a refresh that changes
// nothing.
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
	// The price line is not published either, and that is the same fact from
	// the other side: this program did not value the position FROM that price,
	// so putting it on the row would show a number the empty cell beside it is
	// not derived from.
	if p.Price != nil || p.PriceOn != nil {
		t.Errorf("price/price_on = %v/%v, want both null: no valuation was struck from this quote", p.Price, p.PriceOn)
	}
}

// TestPositionMarketValueGapPrefersTheUnpricedTypeToTheMissingQuote is the
// ordering rule, and it is the same one InBaseGap follows: the cause an
// arriving quote would NOT close is reported ahead of the cause it would.
// Both statements are true of this row — there is no quote AND the type is not
// priced — and answering `no_quote` would promise that a quote is what stands
// between this row and a figure. It is not; nothing does.
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

// TestPositionMarketValueGapNamesTheMissingFaceValue is the third cause, and
// the one #78 does not mention while the plan's own definition of done does
// («Нет котировки» is to be said only where there is no quote). A bond's quote
// is a PERCENTAGE of face value, so with no face value recorded there is
// nothing to take the percentage of — and the quote sitting right there is not
// what is missing.
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

// TestPositionMarketValueGapPrefersTheMissingFaceValueToTheMissingQuote is the
// ordering rule again, on the pair that can both be true of one bond. A quote
// arriving for a bond with no face value closes nothing, so the face value is
// what gets named.
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

// TestPositionMarketValueGapNullWhenTheValuationIsStruck is the negative half
// of all of the above: a share this program prices, with a quote, in its own
// currency. There is a figure, so there is nothing to explain, and a gap
// published here would put a dash-caption over a number.
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
