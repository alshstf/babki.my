package tinvest

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/money"
)

// noteConvertedAt marks an entry restated in its position's currency: the
// amount the broker reported, its currency and the day of the official rate.
const noteConvertedAt = "пересчитано из %s %s по курсу ЦБ на %s"

// noteConversionLeg marks the two exchange entries that keep the account's
// money in the currency it really moved in.
const noteConversionLeg = "обмен по курсу ЦБ для операции в другой валюте"

// convertToPositionCurrency restates the entries that move a paper in a
// currency other than its position's (decision Р-13): a purchase of a dollar
// fund paid in roubles, a fund redeemed in roubles, a yuan bond repaid in
// roubles. The position keeps one currency, as the broker keeps it.
//
//   - A purchase, sale, redemption or repayment is restated at the official
//     rate of its own day — amount, commission and price — and a pair of
//     exchange entries at the same rate is added beside it, so the account's
//     money moves in the currency it really moved in and nowhere else.
//   - A transfer moves no money: only its currency is restated.
//
// The position's currency is the one its first entry that fixes it carries
// (see portfolio's own rule: anything but income, tax, fee, sale or
// redemption). A day with no official rate leaves the row a visible unparsed
// one, until the rates arrive.
func (r *Rebuilder) convertToPositionCurrency(ctx context.Context, p *projected) error {
	positions := map[holdingKey]string{}
	refused := map[uuid.UUID]bool{}
	nextLeg := map[uuid.UUID]int{}
	for _, d := range p.want {
		if d.leg+1 > nextLeg[d.rowID] {
			nextLeg[d.rowID] = d.leg + 1
		}
	}
	var added []desired
	for i := range p.want {
		d := &p.want[i]
		if d.op.InstrumentID == nil || refused[d.rowID] {
			continue
		}
		key := holdingKey{account: d.op.AccountID, instrument: *d.op.InstrumentID}
		pc, known := positions[key]
		if !known {
			if fixesPositionCurrency(d.op) {
				positions[key] = d.op.Currency
			}
			continue
		}
		if d.op.Currency == pc {
			continue
		}
		switch d.op.Type {
		case operation.TypeTransferIn, operation.TypeTransferOut:
			d.op.Currency = pc
		case operation.TypeBuy, operation.TypeSell, operation.TypeRedemption, operation.TypeAmortization:
			legs, refusal, err := r.restate(ctx, d, pc, &nextLeg)
			if err != nil {
				return err
			}
			if refusal != nil {
				p.verdicts[d.rowID] = UnparsedVerdict{Reason: string(refusal.Reason), Detail: refusal.Detail}
				refused[d.rowID] = true
				continue
			}
			added = append(added, legs...)
		}
	}
	for _, d := range added {
		p.rowOf[*d.op.ExternalID] = d.rowID
	}
	kept := p.want[:0]
	for _, d := range p.want {
		if refused[d.rowID] {
			delete(p.rowOf, *d.op.ExternalID)
			continue
		}
		kept = append(kept, d)
	}
	p.want = append(kept, added...)
	sortDesired(p.want)
	return nil
}

// fixesPositionCurrency mirrors the engine's rule for which entry settles a
// position's currency.
func fixesPositionCurrency(op operation.Operation) bool {
	switch op.Type {
	case operation.TypeDividend, operation.TypeCoupon, operation.TypeTax, operation.TypeFee,
		operation.TypeSell, operation.TypeRedemption:
		return false
	}
	return op.AmountMinor != 0 || op.FeeMinor != 0
}

// restate turns d into its position's currency pc at the official rate of its
// day and returns the two exchange entries that go beside it.
func (r *Rebuilder) restate(ctx context.Context, d *desired, pc string, nextLeg *map[uuid.UUID]int) ([]desired, *UnparsedError, error) {
	from := d.op.Currency
	if r.resolver.rates == nil {
		return nil, &UnparsedError{
			Reason: ReasonForeignCurrencyNoRate,
			Detail: fmt.Sprintf("an operation in %s on a position kept in %s, and no official rates to restate it with", from, pc),
		}, nil
	}
	rate, _, err := r.resolver.rates.Rate(ctx, from, pc, d.op.OccurredOn)
	if errors.Is(err, marketdata.ErrNoRate) || (err == nil && !rate.IsPositive()) {
		return nil, &UnparsedError{
			Reason: ReasonForeignCurrencyNoRate,
			Detail: fmt.Sprintf("an operation in %s on a position kept in %s, and no official %s/%s rate for %s yet",
				from, pc, from, pc, d.op.OccurredOn.Format("2006-01-02")),
		}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	convert := func(minor int64) (int64, *UnparsedError) {
		v, err := money.Minor(decimal.NewFromInt(minor).Mul(rate))
		if err != nil {
			return 0, &UnparsedError{Reason: ReasonAmountOutOfBounds, Detail: fmt.Sprintf("%d %s restated in %s: %v", minor, from, pc, err)}
		}
		return v, nil
	}
	amount, refusal := convert(d.op.AmountMinor)
	if refusal != nil {
		return nil, refusal, nil
	}
	fee, refusal := convert(d.op.FeeMinor)
	if refusal != nil {
		return nil, refusal, nil
	}
	// What the account's money did, in each currency: the entry's own effect
	// in the currency it really moved in, and its exact opposite in the
	// position's — so the restated entry and the exchange together leave the
	// position's currency untouched.
	movedFrom := d.op.AmountMinor - d.op.FeeMinor
	movedTo := -(amount - fee)

	note := fmt.Sprintf(noteConvertedAt, decimal.New(d.op.AmountMinor, -2).StringFixed(2), from, d.op.OccurredOn.Format("02.01.2006"))
	d.op.Note = withNote(d.op.Note, note)
	if d.op.Price != nil {
		price := d.op.Price.Mul(rate).Round(10)
		d.op.Price = &price
	}
	d.op.AmountMinor, d.op.FeeMinor, d.op.Currency = amount, fee, pc

	if movedFrom == 0 && movedTo == 0 {
		return nil, nil, nil
	}
	leg := func(currency string, minor int64) desired {
		n := (*nextLeg)[d.rowID] + 1
		(*nextLeg)[d.rowID] = n
		id := fmt.Sprintf("%s%d", externalIDPrefix(d.rowID), n)
		op := operation.Operation{
			AccountID: d.op.AccountID, Type: operation.TypeConversion, OccurredOn: d.op.OccurredOn,
			OccurredAt: d.op.OccurredAt, Currency: currency, AmountMinor: minor,
			Note: noteConversionLeg, TradingMode: d.op.TradingMode, Source: Source, ExternalID: &id,
		}
		return desired{op: op, rowID: d.rowID, at: d.at, leg: n - 1, linkID: d.linkID, parentBrokerID: d.parentBrokerID}
	}
	return []desired{leg(from, movedFrom), leg(pc, movedTo)}, nil, nil
}
