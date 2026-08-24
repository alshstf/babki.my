// Package corporateaction is the registry of what happened to a SECURITY —
// splits, conversions, spin-offs — and the machinery that carries those facts
// into the journals of the accounts that held it.
//
// # Why it exists
//
// A broker reports operations. A corporate action is not an operation, and the
// broker's API says so itself: T-Invest's operation enum carries 71 values and
// none of them is a split, a conversion or a spin-off, while their own FAQ
// states that a split changes the quantity in the portfolio, changes no
// identifier, and arrives through no method a client can poll. So an importer
// that reads operations faithfully still ends up holding one Amazon share where
// the broker holds twenty — wrong by exactly the ratio, with nothing anywhere
// saying why.
//
// # Why it is not per account
//
// A split happens to the paper. Every holder of it, at every broker, in every
// household, wakes up to the same multiplied quantity on the same day. So the
// fact is stored once, keyed by the ISIN (the identity the catalog itself moved
// to — see migration 0020), and what each account does about it is DERIVED: a
// journal row of type split, carrying source "registry", written into every
// account that held the paper at the start of the effective day.
//
// # Derived, and therefore recomputable
//
// Nothing here is incremental. Materialize computes the journal rows the
// registry now asks for, compares them against the rows it wrote last time, and
// applies the difference — the same shape the T-Invest rebuild uses against its
// mirror, and for the same reason: a rule that changes, an event that is
// corrected, and an account that only now bought the paper all reach the journal
// by one path instead of three.
//
// # What is here and what is not
//
// All three kinds are materialized. A split writes one row; a conversion and a
// spin-off write a PAIR — one leg giving up and one receiving, on the same
// account and the same day, built by the journal's own arithmetic (see
// operation.BuildExchange and operation.BuildSpinoff) and applied through the
// same difference the split goes through.
//
// The one thing that still stops a recorded event from reaching a journal is a
// catalog with no row for the paper the event PRODUCES: a journal row points at
// a catalog row, and inventing one would be this program deciding what somebody
// holds. That is a waiting state rather than a failure — the fact was recorded
// the moment it was known, which is the whole reason the registry stores things
// it cannot yet apply (the owner's own funds converted in 2023 and nobody can go
// back and ask the registrar again) — and the event says so on its own row (see
// Event.NotCountedReason). Cataloguing the paper is what completes it: the next
// materialization writes the pair, against the same event, unchanged.
package corporateaction

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/dates"
)

// Kind is what happened to the paper.
type Kind string

const (
	// KindSplit rewrites the quantity of one paper and nothing else: the
	// holding is multiplied, the money spent on it is not touched, and the
	// acquisition dates of the parcels stand. Tax-neutral everywhere this
	// program models (see internal/family/taxresidency.go).
	KindSplit Kind = "split"

	// KindConversion turns one paper into another — a depositary receipt into
	// the share it represented, a fund into its successor. The basis and the
	// acquisition dates travel with it and nothing is realized (НК РФ
	// ст. 214.1 п. 13: the expenses on shares received when receipts are
	// redeemed are the price the receipts were acquired at).
	KindConversion Kind = "conversion"

	// KindSpinOff leaves the original standing and hands out a second paper
	// beside it. Part of the basis moves across, in the proportion BasisShare
	// carries (НК РФ ст. 277 п. 7).
	KindSpinOff Kind = "spin_off"
)

func (k Kind) Valid() bool {
	switch k {
	case KindSplit, KindConversion, KindSpinOff:
		return true
	}
	return false
}

// Materialized reports whether this program carries this KIND into journals.
//
// ALL THREE ARE, SINCE THE PAIRS LANDED — a split writes one row, a conversion
// and a spin-off write two (see Materializer.rowsFor). So this answers true for
// every kind Valid does today, and it is kept as a question of its own rather
// than deleted for the same reason it was written: a fourth kind will be
// recordable before it is applicable, and on that day it must be able to say so
// here instead of somewhere a screen would have to be taught about.
//
// IT SAYS NOTHING ABOUT ONE EVENT. Whether a particular event produced anything
// depends on the accounts (nobody held the paper) and on the catalog (the paper
// it produces has no row here) — see NotCountedReason, which is the per-event
// answer and the one a screen should show beside a row.
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
// for, so no journal row could point at it. It is not a failure and not a
// refusal — the registry deliberately records facts about papers nobody here
// holds — but it IS the difference between "recorded and applied" and "recorded
// and waiting", and a reader who has just entered a conversion is owed it.
// Cured by cataloguing the paper: the next materialization writes the pair.
const NotCountedResultMissing NotCounted = "result_not_in_catalog"

// NotCountedReason answers, for one event, why it is not carried into journals —
// or "" when nothing stands in the way.
//
// resultCataloged says whether the catalog holds the paper this event produces;
// the caller looks that up in one query for a whole list rather than one per row
// (see Store.CatalogedISINs).
func (e Event) NotCountedReason(resultCataloged bool) NotCounted {
	if e.ResultISIN != "" && !resultCataloged {
		return NotCountedResultMissing
	}
	return ""
}

// Source is where a fact came from.
const (
	// SourceMOEX marks a row the exchange job wrote. Not a person's to edit or
	// delete: it is rewritten from the exchange on every run, so an edit would
	// last until the next one and no longer.
	SourceMOEX = "moex_iss"
	// SourceManual marks a row a person recorded, with the evidence in
	// SourceRef.
	SourceManual = "manual"
)

// JournalSource is what the journal rows this package materializes carry in
// their source column. It is the string operation.SourceRegistry holds; the two
// are checked against each other by TestJournalSourceMatchesTheJournalsOwnName
// rather than one importing the other, because the journal must be able to
// name its sources without importing every package that writes one.
const JournalSource = "registry"

// Event is one thing that happened to one paper.
type Event struct {
	ID   uuid.UUID
	Kind Kind
	// ISIN of the paper it happened to. Not an instrument id: the fact
	// outlives any catalog row, and the exchange job records splits of papers
	// nobody here holds.
	ISIN string
	// EffectiveOn is the first day the paper trades in the new quantity at the
	// venue where it is held. The event applies at the START of it: what was
	// held at the close of the day before is multiplied, and a trade dated
	// this day is already in the new quantity. See the migration for the three
	// live events this was checked against.
	EffectiveOn time.Time
	// RatioFrom/RatioTo: one unit becomes RatioTo/RatioFrom units. Whole
	// numbers, the shape the exchange publishes, so that 1:3 is 1 and 3 rather
	// than 0.3333333333.
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

// Ratio is the factor a quantity is multiplied by: RatioTo / RatioFrom.
//
// It is computed on demand from the pair rather than stored, so there is one
// number to trust rather than two that can disagree. DivisionPrecision is what
// decimal uses for a division that does not terminate — 1:3 is a real ratio (a
// reverse split of three into one is 1 -> 3 read the other way) and its factor
// has no exact decimal form, so it is quantized to the same scale the journal
// stores a split ratio at (numeric(20,10), see the migrations). The engine
// quantizes the RESULT of applying it as well (portfolio.applySplit), so a
// truncated tail here cannot leave a position on a finer scale than the column.
func (e Event) Ratio() decimal.Decimal {
	return decimal.NewFromInt(e.RatioTo).DivRound(decimal.NewFromInt(e.RatioFrom), ratioScale)
}

// ratioScale is the number of decimal places a split ratio is kept at, and it
// is the journal column's own scale (operations.split_ratio is
// numeric(20,10)). Written as the engine's constant rather than as 10, because
// the reason it is ten is that the journal stores it that way.
const ratioScale = 10

// ErrNotEditable is what a caller gets for trying to remove a row the exchange
// wrote. Wrapping family.ErrValidation makes it a 400 that names the rule,
// rather than the generic 500 an unmapped error becomes.
var ErrNotEditable = fmt.Errorf(
	"%w: this event came from the exchange and is refreshed from it; only a hand-recorded event can be removed",
	family.ErrValidation)

// ErrDuplicate means the registry already holds an event of this kind for this
// paper on this day. It is a validation error for the same contract reason
// instrument.ErrTickerTaken is one: POST /api/v1/instrument-events declares 400
// and 403 and no 409.
var ErrDuplicate = fmt.Errorf(
	"%w: the registry already holds an event of this kind for this paper on this date",
	family.ErrValidation)

// maxRatio bounds each half of the ratio.
//
// THE BOUND IS THE JOURNAL'S, not a judgement about corporate actions: the
// factor these two produce lands in operations.split_ratio, and the write path
// refuses a ratio at or above 10^10 (see operation.maxSplitRatio) because a
// split multiplies a whole position and a mis-scaled field carries an ordinary
// holding past anything a screen can value. Bounding each half at 10^9 keeps
// every ratio this table can express inside that: the largest is 10^9/1, an
// order of magnitude below the refusal, and the smallest is 1/10^9, which
// rounds to zero at ten decimal places and is refused by validate below rather
// than stored as a factor of nothing.
//
// The real ones are nowhere near: the deepest reverse split this program has
// met is VTBR's 5000:1 (MOEX ISS, 2024-07-15).
const maxRatio = 1_000_000_000

// Validate is what an event has to be, wherever it comes from. Both doors run
// it — the API and the exchange job — so a fact the exchange publishes is held
// to the same rules a person's is, and a row that could not be materialized
// cannot be stored in the first place.
func (e Event) Validate() error {
	if !e.Kind.Valid() {
		return fmt.Errorf("%w: kind must be one of split, conversion, spin_off", family.ErrValidation)
	}
	if e.ISIN == "" {
		return fmt.Errorf("%w: isin is required", family.ErrValidation)
	}
	if e.ISIN == e.ResultISIN {
		return fmt.Errorf("%w: result_isin names the same paper as isin", family.ErrValidation)
	}
	if e.EffectiveOn.IsZero() {
		return fmt.Errorf("%w: effective_on is required", family.ErrValidation)
	}
	// The same ceiling the journal holds an operation to, and for the same
	// reason: this event becomes a journal row dated this day, and a date the
	// journal would refuse is a fact that could never be carried into it. There
	// is deliberately no floor of its own — the journal's own (1900) applies at
	// the moment the row is written, and a registry that refused an older date
	// would be inventing a second rule about how far back history goes.
	if e.EffectiveOn.After(dates.LatestRecordable()) {
		return fmt.Errorf("%w: effective_on must not be in the future", family.ErrValidation)
	}
	if e.RatioFrom < 1 || e.RatioTo < 1 || e.RatioFrom > maxRatio || e.RatioTo > maxRatio {
		return fmt.Errorf("%w: ratio_from and ratio_to must be whole numbers from 1 to %d",
			family.ErrValidation, maxRatio)
	}
	if e.RatioFrom == e.RatioTo {
		return fmt.Errorf("%w: a ratio of %d to %d changes nothing", family.ErrValidation, e.RatioFrom, e.RatioTo)
	}
	// A factor that rounds away at the scale the journal keeps would multiply
	// every holding by zero — the position would vanish and the money spent on
	// it would stay, which is the one shape the engine cannot express (see
	// portfolio.applySplit).
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
		if !e.BasisShare.IsPositive() || !e.BasisShare.LessThan(decimal.NewFromInt(1)) {
			return fmt.Errorf("%w: basis_share must be greater than 0 and less than 1", family.ErrValidation)
		}
	}
	switch e.Source {
	case SourceMOEX:
	case SourceManual:
		if e.SourceRef == "" {
			return fmt.Errorf("%w: a hand-recorded event must link to the evidence for it", family.ErrValidation)
		}
	default:
		return fmt.Errorf("%w: source must be %s or %s", family.ErrValidation, SourceMOEX, SourceManual)
	}
	return nil
}

// errNoSuchEvent is what Delete answers for an id the registry does not hold.
var errNoSuchEvent = errors.New("corporateaction: no such event")
