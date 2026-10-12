// Package allocation keeps the family's target shares of its worth and sets
// them against the worth as it is (decision Р-37): by kind of asset — the
// part the suggestions to top up or rebalance bring to the targets — and by
// currency, for the record only. The worth is the structure's (internal/
// structure) less the money to live on and the brokers' accounts known only
// by their balance.
package allocation

import (
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/structure"
)

// Dimension is what a target is set by.
type Dimension string

const (
	ByKind     Dimension = "kind"
	ByCurrency Dimension = "currency"
)

// Mode is how the family brings its worth to the targets: by top-ups only
// (variant Б of Р-37), or by buying and selling (variant В).
type Mode string

const (
	ModeTopUp     Mode = "topup"
	ModeRebalance Mode = "rebalance"
)

// Target is a key's share of the worth, in percent.
type Target struct {
	Key     string
	Percent decimal.Decimal
}

// Row is a key's part of the worth against its target: Share and Target in
// percent, Gap the share's distance from the target in points, Off what the
// part is above (positive) or below its target in the base currency. Target
// nil: none set.
type Row struct {
	Key    string
	Minor  int64
	Share  decimal.Decimal
	Target *decimal.Decimal
	Gap    *decimal.Decimal
	Off    *int64
}

// View is the worth against the targets.
type View struct {
	BaseCurrency string
	Mode         Mode
	// Worth is what the targets share out; Everyday and Unsplit what is left
	// out of it.
	Worth             int64
	Everyday, Unsplit int64
	Kinds, Currencies []Row
	Unpriced          int
	MissingRates      []string
}

var hundred = decimal.NewFromInt(100)

// rows sets a breakdown against its targets: every key held or targeted, in
// order's order first, then the rest largest first.
func rows(worth int64, slices []structure.Slice, targets []Target, order []string) []Row {
	held := map[string]int64{}
	for _, s := range slices {
		held[s.Key] = s.Minor
	}
	set := map[string]decimal.Decimal{}
	for _, t := range targets {
		set[t.Key] = t.Percent
	}
	var keys []string
	seen := map[string]bool{}
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for _, k := range order {
		if _, ok := held[k]; ok {
			add(k)
		} else if _, ok := set[k]; ok {
			add(k)
		}
	}
	for _, s := range slices {
		add(s.Key)
	}
	for _, t := range targets {
		add(t.Key)
	}
	out := make([]Row, 0, len(keys))
	for _, k := range keys {
		r := Row{Key: k, Minor: held[k], Share: decimal.Zero}
		if worth > 0 {
			r.Share = decimal.NewFromInt(r.Minor).Mul(hundred).Div(decimal.NewFromInt(worth)).Round(2)
		}
		if p, ok := set[k]; ok {
			target := p
			gap := r.Share.Sub(p).Round(2)
			off := r.Minor - decimal.NewFromInt(worth).Mul(p).Div(hundred).Round(0).IntPart()
			r.Target, r.Gap, r.Off = &target, &gap, &off
		}
		out = append(out, r)
	}
	return out
}

// view lays the structure against the targets.
func view(s structure.Structure, mode Mode, kinds, currencies []Target) View {
	order := make([]string, 0, len(structure.Kinds))
	for _, k := range structure.Kinds {
		order = append(order, string(k))
	}
	return View{
		BaseCurrency: s.BaseCurrency, Mode: mode, Worth: s.Targets, Everyday: s.Everyday, Unsplit: s.Unsplit,
		Kinds:      rows(s.Targets, s.ByKind, kinds, order),
		Currencies: rows(s.Targets, s.ByTargetCurrency, currencies, []string{s.BaseCurrency}),
		Unpriced:   s.Unpriced, MissingRates: s.MissingRates,
	}
}
