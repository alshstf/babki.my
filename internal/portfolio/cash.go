package portfolio

import (
	"fmt"
	"sort"
	"time"

	"babki.my/babki/internal/platform/money"
)

// CashPosition is the money an account holds in ONE currency — the balance, and
// the parcels it is made of.
//
// IT IS A HOLDING LIKE ANY OTHER, and that is the whole idea behind computing
// it. Yuan sitting on a Russian broker's account is not a neutral fact: it was
// bought at some rate and is worth another today, and the difference is real
// money the owner has made or lost. Before this, the only cash figure this
// program had was a snapshot the broker named at the last reconciliation — one
// currency, no history, no cost — so that gain was on no screen at all, and the
// account's own total said in as many words that it did not include it.
//
// LOTS ARE WHAT MAKE IT MORE THAN A BALANCE. Each inflow is a parcel with the
// day it arrived; each outflow consumes parcels oldest-first, the same queue
// every other holding here uses. What the parcels are WORTH is not decided here
// — this package holds no rates by design — but a parcel that knows its day can
// be valued at that day's rate by the layer above, exactly as a share's lots
// are (see lotTerms).
type CashPosition struct {
	Currency string
	// Minor is the balance: every cash effect in this currency, added up. It
	// CAN BE NEGATIVE, and that is reported rather than clamped — an account
	// whose journal is missing an operation (a currency purchase the broker
	// would not explain, say) genuinely shows money spent that never arrived,
	// and a floor at zero would hide exactly the discrepancy a reader needs.
	Minor int64
	// Lots are the inflows still held, oldest first. Their sum equals Minor
	// whenever Minor is positive; on a negative balance there are none, since
	// nothing is held.
	Lots []CashLot
}

// CashLot is one arrival of money: how much, and the day it came.
type CashLot struct {
	Minor int64
	On    time.Time
}

// cashEffect is what one journal entry does to the account's money, in the
// currency the entry is denominated in.
//
// THE FORMULA IS AMOUNT LESS FEE, and it is the same one the reconciliation
// compares against the broker: a purchase's amount is the price paid and its
// commission is charged on top, a disposal's amount is the proceeds and its
// commission comes out of them. Both directions are therefore amount − fee.
//
// ok is false for the entries that move no money at all, and each is a
// deliberate exclusion rather than an omission:
//
//   - transfer_in / transfer_out carry a COST BASIS in their amount, not cash.
//     Shares moving between accounts change no balance (see Operation.Amount).
//   - split moves nothing and always carries a zero amount.
//
// Everything else moves money, including the types the position engine refuses
// by type: a deposit, a withdrawal, interest credited on the balance, and both
// legs of a conversion are exactly the entries a cash balance is made of.
func cashEffect(o Operation) (int64, bool, error) {
	switch o.Type {
	case TypeTransferIn, TypeTransferOut, TypeSplit:
		return 0, false, nil
	}
	minor, err := money.Sub(o.AmountMinor, o.FeeMinor)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s on %s, %d less a fee of %d",
			err, o.Type, o.OccurredOn.Format("2006-01-02"), o.AmountMinor, o.FeeMinor)
	}
	return minor, true, nil
}

// Cash folds a journal into one position per currency it holds.
//
// PURE, LIKE Compute, and for the same reason: no rates, no clock, no database.
// What a parcel of yuan was worth in rubles on the day it arrived is a question
// this package cannot answer and does not try to; it answers what arrived, how
// much, and when.
//
// THE ENTRIES ARE EXPECTED IN THE ENGINE'S OWN ORDER (by day, then by the order
// they were recorded) — the same order Compute needs — because the queue is
// only meaningful if the arrivals are seen in the order they happened.
//
// A currency appears in the result as soon as one entry names it, even if its
// balance comes to nought: an account that bought dollars and sold them all
// again has held dollars, and saying nothing about them would be a different
// claim from saying the balance is zero.
func Cash(ops []Operation) (map[string]*CashPosition, error) {
	out := make(map[string]*CashPosition)
	for _, o := range ops {
		effect, moves, err := cashEffect(o)
		if err != nil {
			return nil, err
		}
		if !moves {
			continue
		}
		p, ok := out[o.Currency]
		if !ok {
			p = &CashPosition{Currency: o.Currency}
			out[o.Currency] = p
		}
		balance, err := money.Add(p.Minor, effect)
		if err != nil {
			return nil, fmt.Errorf("%w: the %s balance, adding %d to %d", err, o.Currency, effect, p.Minor)
		}
		p.Minor = balance
		switch {
		case effect > 0:
			p.Lots = append(p.Lots, CashLot{Minor: effect, On: o.OccurredOn})
		case effect < 0:
			p.spend(-effect)
		}
	}
	return out, nil
}

// spend consumes minor units from the oldest parcels first.
//
// SPENDING MORE THAN IS HELD IS NOT AN ERROR HERE, unlike selling more shares
// than the position holds (ErrOversell). A share position that goes negative is
// a broken journal — shares cannot be conjured — while a cash balance that does
// is the ordinary consequence of an operation this program could not record:
// the owner's own account spends yuan it never saw bought, because the purchase
// is one of the trades the broker would not explain. Refusing there would take
// down the whole screen over a gap that is already reported as an unparsed row;
// what the balance does instead is go negative and say so.
func (p *CashPosition) spend(minor int64) {
	for minor > 0 && len(p.Lots) > 0 {
		l := &p.Lots[0]
		if l.Minor > minor {
			l.Minor -= minor
			return
		}
		minor -= l.Minor
		p.Lots = p.Lots[1:]
	}
}

// CashByCurrency returns the positions ordered by currency code, which is what
// a screen shows and what a test can write down. The map itself is the fold's
// working shape; nothing published from it should depend on Go's map order.
func CashByCurrency(positions map[string]*CashPosition) []*CashPosition {
	out := make([]*CashPosition, 0, len(positions))
	for _, p := range positions {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}
