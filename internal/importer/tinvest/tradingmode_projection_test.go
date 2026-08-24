package tinvest

import "testing"

// TestProjectRowCarriesTheTradingModeToTheJournal: the broker says where every
// operation happened — the board an order was matched on, or the venue a deal
// was struck away from an order book — and the journal row now says it too.
//
// It is the owner's own question. After trading in foreign papers was
// suspended the broker opened over-the-counter dealing, and eleven of his
// operations in the FinEx funds carry FINEX_OTC where the rest carry ordinary
// exchange boards; until this, the journal made an off-exchange purchase
// indistinguishable from every other one.
func TestProjectRowCarriesTheTradingModeToTheJournal(t *testing.T) {
	t.Run("the broker's own code, unchanged", func(t *testing.T) {
		row := mirrorRowFor(t, "buy.json")
		row.ClassCode = "FINEX_OTC"
		op := projectOne(t, row, resolvedShare())
		if op.TradingMode == nil || *op.TradingMode != "FINEX_OTC" {
			t.Errorf("trading mode = %v, want FINEX_OTC — the broker's word, carried and not translated",
				op.TradingMode)
		}
	})

	// NOTHING IS PUT THERE WHEN THE BROKER SAID NOTHING, and that is ordinary
	// rather than exceptional: money moving in and out of an account describes
	// no instrument and carries no mode — 83 deposits and 52 withdrawals on
	// the owner's own account. A row carrying a mode of "" would be a row
	// claiming to know where a deposit was executed.
	t.Run("nothing when the broker named none", func(t *testing.T) {
		row := mirrorRowFor(t, "buy.json")
		row.ClassCode = ""
		op := projectOne(t, row, resolvedShare())
		if op.TradingMode != nil {
			t.Errorf("trading mode = %q, want nothing at all", *op.TradingMode)
		}
	})
}
