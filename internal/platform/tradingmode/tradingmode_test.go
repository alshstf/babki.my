package tradingmode_test

import (
	"strings"
	"testing"

	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/tradingmode"
)

// Each Moscow Exchange row's kind agrees with the last word of the
// exchange's own title for the board: «безадрес.» or «адрес.».
func TestEveryClaimCitesItsAuthority(t *testing.T) {
	for code, fact := range tradingmode.Modes {
		switch fact.Why {
		case tradingmode.MoexISS:
			if fact.MoexTitle == "" {
				t.Errorf("%s cites the exchange's board index and quotes no title from it", code)
				continue
			}
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
			// CodeNamesItself is only for a code that says OTC.
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

// Codes seen on the owner's account with no source stay Unknown; adding one
// with a guessed label must come with a source.
func TestUnnamedCodesAreAnsweredUnknown(t *testing.T) {
	for _, code := range []string{"SPBXM", "SPBOPT", "BQUOTE_SHR", "A29", "PSSU", "FAKE_OLD_MEX"} {
		if got := tradingmode.Of(code); got != tradingmode.Unknown {
			t.Errorf("Of(%q) = %q, want unknown — this program has no source for what that code is, "+
				"and a label without a source is exactly what this table refuses to hold", code, got)
		}
	}
}

// Answers written as literals, not read back from the table.
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

// Every Kind is a value of the contract's enum.
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
