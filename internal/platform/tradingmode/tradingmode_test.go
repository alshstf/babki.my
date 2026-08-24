package tradingmode_test

import (
	"strings"
	"testing"

	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/tradingmode"
)

// TestEveryClaimCitesItsAuthority is what keeps this package's table from
// becoming a list of opinions.
//
// Every row says a code is one kind of mode rather than another, and every row
// carries the evidence for saying so. The two are checked against each other
// here: a Moscow Exchange board is classified BY THE EXCHANGE'S OWN TITLE for
// it — whose last word is «безадрес.» for an anonymous order book and «адрес.»
// for a negotiated deal — so the kind is read off the citation rather than
// judged. A row whose title stopped agreeing with its kind would be a row
// where someone changed the claim and left the evidence behind.
func TestEveryClaimCitesItsAuthority(t *testing.T) {
	for code, fact := range tradingmode.Modes {
		switch fact.Why {
		case tradingmode.MoexISS:
			if fact.MoexTitle == "" {
				t.Errorf("%s cites the exchange's board index and quotes no title from it", code)
				continue
			}
			// The exchange's own last word, and the only thing this program
			// reads from the title.
			anonymous := strings.Contains(fact.MoexTitle, "безадрес")
			addressed := strings.Contains(fact.MoexTitle, "адрес") && !anonymous
			switch {
			case anonymous && fact.Kind != tradingmode.OrderBook:
				t.Errorf("%s is titled %q — «безадрес.», an order book — but is classified %q",
					code, fact.MoexTitle, fact.Kind)
			case addressed && fact.Kind != tradingmode.Negotiated:
				t.Errorf("%s is titled %q — «адрес.», a negotiated deal — but is classified %q",
					code, fact.MoexTitle, fact.Kind)
			case !anonymous && !addressed:
				t.Errorf("%s quotes the title %q, which says neither «безадрес.» nor «адрес.», "+
					"so the exchange's own words do not decide this row's kind and something else did",
					code, fact.MoexTitle)
			}
			// An exchange's board is never off-exchange, whatever else it is.
			if fact.Kind == tradingmode.OffExchange {
				t.Errorf("%s is a board of the exchange's own index and is classified off_exchange", code)
			}
		case tradingmode.CodeNamesItself:
			// The one evidence that rests on the code rather than on a
			// register: it may be used ONLY where the code really does say
			// what it is, which for this program means naming itself OTC.
			if !strings.Contains(code, "OTC") {
				t.Errorf("%s claims its own name is the evidence, but the name says nothing: "+
					"this evidence is for a code that states its nature, not for one that merely looks familiar", code)
			}
			if fact.Kind != tradingmode.OffExchange {
				t.Errorf("%s names itself OTC and is classified %q", code, fact.Kind)
			}
		default:
			t.Errorf("%s claims no authority at all (%q) — a row without one belongs outside this table, "+
				"where it is answered as unknown and its code is shown as it is", code, fact.Why)
		}
	}
}

// TestUnnamedCodesAreAnsweredUnknown: the codes on the owner's own account
// that this program cannot source. Each is real live data and each is
// deliberately absent from the table — the Saint Petersburg boards, the two
// off-exchange-looking codes from 2023, the fourth PS* board whose siblings
// ARE in the exchange's index, and the broker's own placeholder.
//
// The test exists because "we do not know" is an ANSWER here and must not
// quietly become a guess: adding any of these to the table with a plausible
// label turns this red, and the reviewer then has to produce the source.
func TestUnnamedCodesAreAnsweredUnknown(t *testing.T) {
	for _, code := range []string{"SPBXM", "SPBOPT", "BQUOTE_SHR", "A29", "PSSU", "FAKE_OLD_MEX"} {
		if got := tradingmode.Of(code); got != tradingmode.Unknown {
			t.Errorf("Of(%q) = %q, want unknown — this program has no source for what that code is, "+
				"and a label without a source is exactly what this table refuses to hold", code, got)
		}
	}
}

// TestOfNamesTheModesItCan walks the ones it can, with the answers written out
// as literals rather than read back out of the table the code under test uses:
// a test that asks the table what the table says would agree with it however
// wrong it became.
func TestOfNamesTheModesItCan(t *testing.T) {
	for code, want := range map[string]tradingmode.Kind{
		// Moscow Exchange, anonymous.
		"TQBR": tradingmode.OrderBook,
		"TQCB": tradingmode.OrderBook,
		"TQTF": tradingmode.OrderBook,
		"CETS": tradingmode.OrderBook,
		// Moscow Exchange, addressed — still the exchange.
		"CNGD": tradingmode.Negotiated,
		"PSAU": tradingmode.Negotiated,
		"PSBB": tradingmode.Negotiated,
		// The broker's own over-the-counter dealing.
		"FINEX_OTC": tradingmode.OffExchange,
		// Nobody said anything at all.
		"": tradingmode.Unknown,
	} {
		if got := tradingmode.Of(code); got != want {
			t.Errorf("Of(%q) = %q, want %q", code, got, want)
		}
	}
}

// TestEveryKindIsAContractValue: the kinds this package decides are the kinds
// the API publishes, and the two are separate declarations — one in Go, one
// generated from the contract. A value renamed on either side without the
// other would reach a client as a word its schema forbids, and the client
// would show a row it cannot render rather than the mode it asked for.
func TestEveryKindIsAContractValue(t *testing.T) {
	for _, kind := range []tradingmode.Kind{
		tradingmode.OrderBook, tradingmode.Negotiated,
		tradingmode.OffExchange, tradingmode.Unknown,
	} {
		if !apitypes.TradingModeKind(kind).Valid() {
			t.Errorf("%q is not a value of the contract's TradingModeKind enum", kind)
		}
	}
}
