// Package corporateaction is the registry of what happened to a security
// (splits, conversions, spin-offs) and the machinery that carries those facts
// into the journals of the accounts that held it.
//
// Brokers report operations, and a corporate action is not one: T-Invest's
// operation enum has no split, conversion or spin-off, and its FAQ says a split
// arrives through no method a client can poll. Without this registry an importer
// holds one Amazon share where the broker holds twenty.
//
// A split happens to the paper, so the fact is stored once, keyed by ISIN, and
// each account's share of it is derived: journal rows with source "registry" in
// every account that held the paper at the start of the effective day. A split
// writes one row; a conversion or a spin-off writes a pair built by
// operation.BuildExchange or operation.BuildSpinoff.
//
// Materialize is not incremental: it computes the rows the registry asks for,
// compares them with the rows it wrote last time, and applies the difference, so
// a changed rule, a corrected event and a newly bought paper all take one path.
//
// An event whose resulting paper has no catalog row waits rather than fails
// (Event.NotCountedReason): the fact is kept, since it may not be recoverable
// later, and cataloguing the paper lets the next materialization write the
// pair.
package corporateaction

import (
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/platform/dates"
)

// Kind is what happened to the paper.
type Kind string

const (
	// KindSplit rewrites one paper's quantity: the holding is multiplied, the
	// money and acquisition dates stand. Tax-neutral everywhere this program
	// models (see internal/family/taxresidency.go).
	KindSplit Kind = "split"

	// KindConversion turns one paper into another (a receipt into its share,
	// a fund into its successor). Basis and acquisition dates travel with it and
	// nothing is realized (НК РФ ст. 214.1 п. 13).
	KindConversion Kind = "conversion"

	// KindSpinOff leaves the original standing and adds a second paper.
	// BasisShare of the basis moves across: 0 by default, as the broker keeps it
	// (Р-16), since the tax code does not divide it for an individual.
	KindSpinOff Kind = "spin_off"
)

// kinds is every kind; the schema's CHECK names the same, held together by a
// test.
var kinds = []Kind{KindSplit, KindConversion, KindSpinOff}

func (k Kind) Valid() bool { return slices.Contains(kinds, k) }

// Materialized reports whether this program carries this kind into journals.
// All three are today; a future kind may be recordable before it is applicable.
// Whether one event produced anything is NotCountedReason's question.
func (k Kind) Materialized() bool {
	switch k {
	case KindSplit, KindConversion, KindSpinOff:
		return true
	}
	return false
}

// NotCounted is why one recorded event still puts nothing in any journal.
type NotCounted string

// NotCountedResultMissing: the event produces a paper the catalog has no row
// for, so no journal row can point at it. Not a failure: the event waits until
// the paper is catalogued.
const NotCountedResultMissing NotCounted = "result_not_in_catalog"

// NotCountedReason says why one event is not carried into journals, or "" if
// nothing stands in the way. resultCataloged comes from one query for a whole list
// (Store.CatalogedISINs).
func (e Event) NotCountedReason(resultCataloged bool) NotCounted {
	if e.ResultISIN != "" && !resultCataloged {
		return NotCountedResultMissing
	}
	return ""
}

// Source is where a fact came from.
const (
	// SourceMOEX marks a row the exchange job wrote. It is rewritten on every
	// run, so it is not a person's to edit or delete.
	SourceMOEX = "moex_iss"
	// SourceYahoo marks a split of a foreign paper the share feed's job wrote;
	// like the exchange's, it is rewritten on every run.
	SourceYahoo = "yahoo"
	// SourceKnown marks a redomiciliation this program knows from the
	// exchange's notice (KnownConversions); rewritten on every run.
	SourceKnown = "known"
	// SourceManual marks a row a person recorded, with the evidence in
	// SourceRef.
	SourceManual = "manual"
)

// JournalSource is the source the materialized journal rows carry, the same
// string as operation.SourceRegistry; TestJournalSourceMatchesTheJournalsOwnName
// holds them together so the journal need not import every writer.
const JournalSource = "registry"

// Event is one thing that happened to one paper.
type Event struct {
	ID   uuid.UUID
	Kind Kind
	// ISIN of the paper, not an instrument id: the fact outlives catalog rows
	// and covers papers nobody here holds.
	ISIN string
	// EffectiveOn is the first day the paper trades in the new quantity. The
	// event applies at the start of it: the previous close is multiplied, and a
	// trade dated this day is already in the new quantity.
	EffectiveOn time.Time
	// One unit becomes RatioTo/RatioFrom units; whole numbers, as the exchange
	// publishes them.
	RatioFrom, RatioTo int64
	// ResultISIN is the paper a conversion or a spin-off produces; empty for a
	// split.
	ResultISIN string
	// BasisShare is the fraction of the original's cost basis a spin-off moves
	// across; nil for the other kinds.
	BasisShare *decimal.Decimal
	Source     string
	SourceRef  string
	MOEXSecID  string
	Note       string
	CreatedAt  time.Time
	CreatedBy  *uuid.UUID
}

// Ratio is RatioTo / RatioFrom, quantized to the journal's split_ratio scale
// (numeric(20,10)), since ratios like 1:3 do not terminate. The engine quantizes
// the result of applying it too (portfolio.applySplit).
func (e Event) Ratio() decimal.Decimal {
	return decimal.NewFromInt(e.RatioTo).DivRound(decimal.NewFromInt(e.RatioFrom), ratioScale)
}

// ratioScale is operations.split_ratio's scale.
const ratioScale = 10

// ErrNotEditable refuses removing a row the exchange wrote: a 400 naming the
// rule.
var ErrNotEditable = fmt.Errorf(
	"%w: this event came from the exchange and is refreshed from it; only a hand-recorded event can be removed",
	family.ErrValidation)

// ErrDuplicate: an event of this kind for this paper on this day already
// exists. A validation error because the endpoint declares 400 and 403, not
// 409.
var ErrDuplicate = fmt.Errorf(
	"%w: the registry already holds an event of this kind for this paper on this date",
	family.ErrValidation)

// maxRatio bounds each half of the ratio by the journal's limit, not a view on
// corporate actions: split_ratio refuses 10^10 and above (operation.maxSplitRatio),
// so 10^9/1 stays inside it, and 1/10^9 rounds to zero and is refused by Validate.
// The deepest real reverse split met so far is VTBR's 5000:1 (MOEX ISS,
// 2024-07-15).
const maxRatio = 1_000_000_000

// Validate is what an event must be, from the API or the exchange job alike,
// so nothing stored is unmaterializable.
func (e Event) Validate() error {
	if !e.Kind.Valid() {
		return fmt.Errorf("%w: kind must be one of split, conversion, spin_off", family.ErrValidation)
	}
	if e.ISIN == "" {
		return fmt.Errorf("%w: isin is required", family.ErrValidation)
	}
	// Matching is by string equality, so another spelling would be another
	// paper.
	if normal, err := instrument.NormalizeISIN(e.ISIN); err != nil || normal != e.ISIN {
		return fmt.Errorf("%w: isin must be an ISIN in upper case, e.g. US0231351067", family.ErrValidation)
	}
	if normal, err := instrument.NormalizeISIN(e.ResultISIN); err != nil || normal != e.ResultISIN {
		return fmt.Errorf("%w: result_isin must be an ISIN in upper case, e.g. US0231351067", family.ErrValidation)
	}
	if e.ISIN == e.ResultISIN {
		return fmt.Errorf("%w: result_isin names the same paper as isin", family.ErrValidation)
	}
	if e.EffectiveOn.IsZero() {
		return fmt.Errorf("%w: effective_on is required", family.ErrValidation)
	}
	// The journal's ceiling, since the event becomes a journal row on this day.
	if e.EffectiveOn.After(dates.LatestRecordable()) {
		return fmt.Errorf("%w: effective_on must not be in the future", family.ErrValidation)
	}
	// The journal's floor: an earlier event would be stored and then refused on
	// every sweep.
	if e.EffectiveOn.Before(dates.EarliestRecordable()) {
		return fmt.Errorf("%w: effective_on must not be earlier than %s",
			family.ErrValidation, dates.EarliestRecordable().Format(time.DateOnly))
	}
	if e.RatioFrom < 1 || e.RatioTo < 1 || e.RatioFrom > maxRatio || e.RatioTo > maxRatio {
		return fmt.Errorf("%w: ratio_from and ratio_to must be whole numbers from 1 to %d",
			family.ErrValidation, maxRatio)
	}
	// A 1:1 split says nothing happened. A 1:1 conversion or spin-off is the
	// common case: TCS Group receipts became МКПАО «ТКС Холдинг» shares one for
	// one on 2024-02-27 (moex.com/n67851), and Т-Капитал's TECH, TSPX and TUSD
	// spin-offs were one for one.
	if e.Kind == KindSplit && e.RatioFrom == e.RatioTo {
		return fmt.Errorf("%w: a split of %d to %d changes nothing", family.ErrValidation, e.RatioFrom, e.RatioTo)
	}
	// A factor that rounds to zero would empty every position and keep its
	// money, which the engine cannot express.
	if e.Ratio().IsZero() {
		return fmt.Errorf("%w: %d to %d is smaller than the journal can record (%d decimal places)",
			family.ErrValidation, e.RatioFrom, e.RatioTo, ratioScale)
	}
	switch e.Kind {
	case KindSplit:
		if e.ResultISIN != "" {
			return fmt.Errorf("%w: a split produces no new paper, so result_isin does not belong on one",
				family.ErrValidation)
		}
		if e.BasisShare != nil {
			return fmt.Errorf("%w: a split moves no cost basis, so basis_share does not belong on one",
				family.ErrValidation)
		}
	case KindConversion:
		if e.ResultISIN == "" {
			return fmt.Errorf("%w: a conversion must name the paper it produces", family.ErrValidation)
		}
		if e.BasisShare != nil {
			return fmt.Errorf("%w: a conversion moves the whole cost basis, so basis_share does not belong on one",
				family.ErrValidation)
		}
	case KindSpinOff:
		if e.ResultISIN == "" {
			return fmt.Errorf("%w: a spin-off must name the paper it produces", family.ErrValidation)
		}
		if e.BasisShare == nil {
			return fmt.Errorf("%w: a spin-off must say what share of the cost basis moves across", family.ErrValidation)
		}
		if e.BasisShare.IsNegative() || !e.BasisShare.LessThan(decimal.NewFromInt(1)) {
			return fmt.Errorf("%w: basis_share must be at least 0 and less than 1", family.ErrValidation)
		}
	}
	switch e.Source {
	case SourceMOEX, SourceYahoo, SourceKnown:
	case SourceManual:
		if e.SourceRef == "" {
			return fmt.Errorf("%w: a hand-recorded event must link to the evidence for it", family.ErrValidation)
		}
		if utf8.RuneCountInString(e.SourceRef) > MaxSourceRefRunes {
			return fmt.Errorf("%w: source_ref must be at most %d characters", family.ErrValidation, MaxSourceRefRunes)
		}
		if utf8.RuneCountInString(e.Note) > MaxNoteRunes {
			return fmt.Errorf("%w: note must be at most %d characters", family.ErrValidation, MaxNoteRunes)
		}
	default:
		return fmt.Errorf("%w: source must be %s, %s, %s or %s", family.ErrValidation,
			SourceMOEX, SourceYahoo, SourceKnown, SourceManual)
	}
	return nil
}

// MaxSourceRefRunes and MaxNoteRunes bound a hand-recorded event's link and
// note, in code points as openapi counts them. The exchange's own texts are not
// held to them.
const (
	MaxSourceRefRunes = 500
	MaxNoteRunes      = 1000
)

// errNoSuchEvent is what Delete answers for an id the registry does not hold.
var errNoSuchEvent = errors.New("corporateaction: no such event")
