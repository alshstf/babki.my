package moex

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/marketdata"
)

// issTable is one table of an ISS answer, columns by name.
type issTable struct {
	Columns []string `json:"columns"`
	Data    [][]any  `json:"data"`
}

// cell is the row's value under the named column; nil when the column or the
// value is missing.
func (t issTable) cell(row []any, name string) any {
	for i, c := range t.Columns {
		if c == name && i < len(row) {
			return row[i]
		}
	}
	return nil
}

// dayCell reads a date cell; nil for an empty one or the exchange's
// «0000-00-00».
func dayCell(v any) (*time.Time, error) {
	s, _ := v.(string)
	if s == "" || strings.HasPrefix(s, "0000") {
		return nil, nil
	}
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return nil, fmt.Errorf("date %q: %w", s, err)
	}
	return &d, nil
}

// numberCell reads a number cell; nil for a missing one, which the exchange
// sends for a floating coupon not yet set.
func numberCell(v any) (*decimal.Decimal, error) {
	if v == nil {
		return nil, nil
	}
	d, err := decimalOf(v)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// BondSchedule is a bond's whole schedule as the exchange publishes it
// (bondization): coupons, partial repayments, the redemption — the repayment
// the exchange marks «maturity» — and offers, per unit, in the face's
// currency. found is false for a bond the exchange does not trade.
func (c *Client) BondSchedule(ctx context.Context, isin, ticker string) ([]marketdata.BondEvent, bool, error) {
	code := isin
	if code == "" {
		code = ticker
	}
	sec, found, err := c.FindSecurity(ctx, code)
	if err != nil || !found || sec.Kind != "bond" {
		return nil, false, err
	}
	var body struct {
		Coupons       issTable `json:"coupons"`
		Amortizations issTable `json:"amortizations"`
		Offers        issTable `json:"offers"`
	}
	q := url.Values{
		"iss.meta": {"off"},
		"iss.only": {"coupons,amortizations,offers"},
		"limit":    {"unlimited"},
	}
	what := "the schedule of " + sec.SecID
	if err := c.getJSON(ctx, "/iss/securities/"+url.PathEscape(sec.SecID)+"/bondization.json?"+q.Encode(), what, &body); err != nil {
		return nil, false, err
	}
	events, err := scheduleEvents(body.Coupons, body.Amortizations, body.Offers)
	if err != nil {
		return nil, false, fmt.Errorf("moex: %s: %w", what, err)
	}
	return events, true, nil
}

// scheduleEvents reads the three tables of a bondization answer.
func scheduleEvents(coupons, amortizations, offers issTable) ([]marketdata.BondEvent, error) {
	var out []marketdata.BondEvent
	read := func(t issTable, kindOf func(row []any) marketdata.BondEventKind, dayCol, recordCol, valueCol, percentCol string) error {
		for _, row := range t.Data {
			on, err := dayCell(t.cell(row, dayCol))
			if err != nil {
				return err
			}
			unit, _ := t.cell(row, "faceunit").(string)
			if on == nil || unit == "" {
				// No day or no currency: nothing to put on a calendar.
				continue
			}
			e := marketdata.BondEvent{Kind: kindOf(row), On: *on, Currency: faceUnit(unit)}
			if recordCol != "" {
				if e.RecordOn, err = dayCell(t.cell(row, recordCol)); err != nil {
					return err
				}
			}
			if e.Value, err = numberCell(t.cell(row, valueCol)); err != nil {
				return fmt.Errorf("%s value: %w", e.Kind, err)
			}
			if percentCol != "" {
				if e.Percent, err = numberCell(t.cell(row, percentCol)); err != nil {
					return fmt.Errorf("%s percent: %w", e.Kind, err)
				}
			}
			out = append(out, e)
		}
		return nil
	}
	if err := read(coupons, func([]any) marketdata.BondEventKind { return marketdata.BondCoupon },
		"coupondate", "recorddate", "value", "valueprc"); err != nil {
		return nil, err
	}
	if err := read(amortizations, func(row []any) marketdata.BondEventKind {
		if s, _ := amortizations.cell(row, "data_source").(string); s == "maturity" {
			return marketdata.BondRedemption
		}
		return marketdata.BondAmortization
	}, "amortdate", "", "value", "valueprc"); err != nil {
		return nil, err
	}
	if err := read(offers, func([]any) marketdata.BondEventKind { return marketdata.BondOffer },
		"offerdate", "", "value", "price"); err != nil {
		return nil, err
	}
	return out, nil
}
