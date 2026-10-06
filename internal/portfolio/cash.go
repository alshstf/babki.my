package portfolio

import (
	"fmt"
	"sort"
	"time"

	"babki.my/babki/internal/platform/money"
)

// CashPosition is the money an account holds in one currency, as a holding:
// bought at one rate and worth another today. Each inflow is a dated parcel and
// each outflow consumes parcels oldest first, so the layer above can value
// parcels at their arrival rates, as it values lots.
type CashPosition struct {
	Currency string
	// Minor is the balance. It can be negative — a journal missing an operation
	// shows money spent that never arrived — and is reported, not clamped.
	Minor int64
	// OverdrawnSince is the first day the journal took the balance below zero,
	// by the operations' own dates; nil unless the balance is negative now.
	OverdrawnSince *time.Time
	// Lots are the inflows still held, oldest first; they sum to Minor when it is
	// positive and are empty when it is not.
	Lots []CashLot
	// Realizations are the money that has left, with the parcels it took and its
	// day — where a banked currency result lives (roubles to dollars at 100 and
	// back at 120 earned 20 000 roubles, while the remaining balances show none).
	// Money spent that was never seen arriving releases nothing; it shows as the
	// negative balance.
	Realizations []CashRealization
	// shortfall is money spent that no parcel covered — the overdraft. Later
	// arrivals pay it off first and are not held; otherwise the parcels would
	// claim more than the balance. Covering an overdraft records no departure: the
	// spending had no known cost.
	shortfall int64
}

// CashRealization is one departure of money: the parcels consumed and the day.
// Valuing it is the layer above's job.
type CashRealization struct {
	OccurredOn time.Time
	Released   []CashLot
}

// Minor is what the departure accounted for: the parcels it took, so proceeds
// and cost are about the same money.
func (r CashRealization) Minor() int64 {
	var total int64
	for _, l := range r.Released {
		total += l.Minor
	}
	return total
}

// CashLot is one arrival of money: how much and the day.
type CashLot struct {
	Minor int64
	On    time.Time
}

// MovesCash reports whether an entry's AmountMinor is money that changed the
// balance. False for legs carrying a cost basis (transfer, conversion,
// spin-off) and for a split; true for everything else, including cash-level
// types the engine skips. Exported for the import's reconciliation too;
// TestMovesCashClassifiesEveryType makes every type answer explicitly.
func MovesCash(o Operation) bool {
	switch o.Type {
	case TypeTransferIn, TypeTransferOut,
		TypeExchangeOut, TypeExchangeIn,
		TypeSpinoffOut, TypeSpinoffIn,
		TypeSplit:
		return false
	}
	return true
}

// cashEffect is what one entry does to the account's money, in its own
// currency: amount less fee, as reconciliation computes it. ok is MovesCash.
func cashEffect(o Operation) (int64, bool, error) {
	if !MovesCash(o) {
		return 0, false, nil
	}
	minor, err := money.Sub(o.AmountMinor, o.FeeMinor)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s on %s, %d less a fee of %d",
			err, o.Type, o.OccurredOn.Format("2006-01-02"), o.AmountMinor, o.FeeMinor)
	}
	return minor, true, nil
}

// Cash folds a journal into one position per currency. It is pure, like
// Compute, and expects the engine's order. A currency appears once any entry
// names it, even at a zero balance.
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
		if balance < 0 && p.OverdrawnSince == nil {
			day := o.OccurredOn
			p.OverdrawnSince = &day
		}
		// Money moves on its settlement day when known (decision Р-3), the day the
		// paper's own basis and proceeds are priced on.
		on := RateDay(o)
		switch {
		case effect > 0:
			p.receive(effect, on)
		case effect < 0:
			released := p.spend(-effect)
			if len(released) > 0 {
				p.Realizations = append(p.Realizations, CashRealization{
					OccurredOn: on, Released: released,
				})
			}
		}
	}
	for _, p := range out {
		if p.Minor >= 0 {
			p.OverdrawnSince = nil
		}
	}
	return out, nil
}

// receive books an arrival: it pays off any overdraft first and holds the
// rest.
func (p *CashPosition) receive(minor int64, on time.Time) {
	if p.shortfall > 0 {
		covered := minor
		if covered > p.shortfall {
			covered = p.shortfall
		}
		p.shortfall -= covered
		minor -= covered
	}
	if minor > 0 {
		p.Lots = append(p.Lots, CashLot{Minor: minor, On: on})
	}
}

// spend consumes minor units from the oldest parcels. Spending more than is
// held is not an error, unlike selling shares not held: a journal missing a
// currency purchase the broker would not explain is ordinary, so the balance
// goes negative and says so.
func (p *CashPosition) spend(minor int64) []CashLot {
	var released []CashLot
	for minor > 0 && len(p.Lots) > 0 {
		l := &p.Lots[0]
		if l.Minor > minor {
			released = append(released, CashLot{Minor: minor, On: l.On})
			l.Minor -= minor
			return released
		}
		released = append(released, *l)
		minor -= l.Minor
		p.Lots = p.Lots[1:]
	}
	// Whatever no parcel covered is an overdraft, paid off by the next arrival.
	p.shortfall += minor
	return released
}

// CashByCurrency returns the positions ordered by currency code, independent
// of map order.
func CashByCurrency(positions map[string]*CashPosition) []*CashPosition {
	out := make([]*CashPosition, 0, len(positions))
	for _, p := range positions {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}
