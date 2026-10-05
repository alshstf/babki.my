package moex

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Amortization is one repayment of part of a bond's face value, as the
// exchange's schedule (bondization) lists it: the day, the amount per bond, and
// the face value the bond was issued at.
type Amortization struct {
	On          time.Time
	Value       decimal.Decimal
	InitialFace decimal.Decimal
	Currency    string
}

// faceUnit is the currency code the schedule writes, in ISO-4217: the exchange
// still writes roubles as SUR or RUR on older issues.
func faceUnit(code string) string {
	switch c := strings.ToUpper(code); c {
	case "SUR", "RUR":
		return "RUB"
	default:
		return c
	}
}

// Amortizations reads a bond's whole repayment schedule — past and future —
// from the exchange, by its exchange code.
func (c *Client) Amortizations(ctx context.Context, secID string) ([]Amortization, error) {
	var body struct {
		Amortizations struct {
			Columns []string `json:"columns"`
			Data    [][]any  `json:"data"`
		} `json:"amortizations"`
	}
	q := url.Values{
		"iss.meta":              {"off"},
		"iss.only":              {"amortizations"},
		"amortizations.columns": {"amortdate,initialfacevalue,faceunit,value"},
		"limit":                 {"unlimited"},
	}
	what := "amortizations of " + secID
	if err := c.getJSON(ctx, "/iss/securities/"+url.PathEscape(secID)+"/bondization.json?"+q.Encode(), what, &body); err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, name := range body.Amortizations.Columns {
		col[name] = i
	}
	for _, name := range []string{"amortdate", "initialfacevalue", "faceunit", "value"} {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("moex: %s: response missing %s", what, name)
		}
	}
	out := make([]Amortization, 0, len(body.Amortizations.Data))
	for _, row := range body.Amortizations.Data {
		day, _ := row[col["amortdate"]].(string)
		on, err := time.Parse(time.DateOnly, day)
		if err != nil {
			return nil, fmt.Errorf("moex: %s: date %q: %w", what, day, err)
		}
		value, err := decimalOf(row[col["value"]])
		if err != nil {
			return nil, fmt.Errorf("moex: %s: value: %w", what, err)
		}
		initial, err := decimalOf(row[col["initialfacevalue"]])
		if err != nil {
			return nil, fmt.Errorf("moex: %s: initial face value: %w", what, err)
		}
		unit, _ := row[col["faceunit"]].(string)
		out = append(out, Amortization{On: on, Value: value, InitialFace: initial, Currency: faceUnit(unit)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].On.Before(out[j].On) })
	return out, nil
}

// decimalOf reads a number the exchange sent as JSON.
func decimalOf(v any) (decimal.Decimal, error) {
	switch n := v.(type) {
	case float64:
		return decimal.NewFromFloat(n), nil
	case string:
		return decimal.NewFromString(n)
	default:
		return decimal.Zero, fmt.Errorf("not a number: %v", v)
	}
}

// FaceBefore is a bond's outstanding face value per unit just before the
// repayment the schedule lists nearest to on, within tolerance either side —
// the broker pays a repayment on its schedule day or a few days after. It is
// the initial face less every repayment scheduled before that one. Not found
// when no repayment is that close, or the schedule is empty.
func FaceBefore(schedule []Amortization, on time.Time, tolerance time.Duration) (decimal.Decimal, string, bool) {
	best := -1
	var gap time.Duration
	for i, a := range schedule {
		d := on.Sub(a.On)
		if d < 0 {
			d = -d
		}
		if d <= tolerance && (best < 0 || d < gap) {
			best, gap = i, d
		}
	}
	if best < 0 {
		return decimal.Zero, "", false
	}
	face := schedule[best].InitialFace
	for _, a := range schedule[:best] {
		face = face.Sub(a.Value)
	}
	if !face.IsPositive() {
		return decimal.Zero, "", false
	}
	return face, schedule[best].Currency, true
}

// scheduleTTL is how long a bond's repayment schedule is reused before it is
// asked for again: it changes when an issuer amends it, which is rare, and a
// sync runs many times a day.
const scheduleTTL = 12 * time.Hour

type cachedSchedule struct {
	at       time.Time
	schedule []Amortization
	found    bool
}

// FaceBeforeByISIN answers a bond's outstanding face value per unit just before
// a repayment on day on (see FaceBefore), finding the bond on the exchange by
// its ISIN. Not found when the exchange does not know the bond or lists no
// repayment near that day. Schedules are reused for scheduleTTL.
func (c *Client) FaceBeforeByISIN(ctx context.Context, isin string, on time.Time) (decimal.Decimal, string, bool, error) {
	c.mu.Lock()
	cached, ok := c.schedules[isin]
	c.mu.Unlock()
	if !ok || time.Since(cached.at) > scheduleTTL {
		sec, found, err := c.FindSecurity(ctx, isin)
		if err != nil {
			return decimal.Zero, "", false, err
		}
		cached = cachedSchedule{at: time.Now(), found: found}
		if found {
			if cached.schedule, err = c.Amortizations(ctx, sec.SecID); err != nil {
				return decimal.Zero, "", false, err
			}
		}
		c.mu.Lock()
		c.schedules[isin] = cached
		c.mu.Unlock()
	}
	if !cached.found {
		return decimal.Zero, "", false, nil
	}
	face, currency, ok := FaceBefore(cached.schedule, on, 10*24*time.Hour)
	return face, currency, ok, nil
}
