package portfolio

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/money"
)

// journalStore is what the service needs from operation.Store, as a local
// interface because package operation imports this one.
type journalStore interface {
	ListForEngine(ctx context.Context, spaceID, accountID uuid.UUID) ([]Operation, error)
}

// quoteStore is what the service needs from marketdata.Store, local so tests
// can control quotes and count round trips.
type quoteStore interface {
	LatestQuotes(ctx context.Context, instrumentIDs []uuid.UUID) (map[uuid.UUID]marketdata.Quote, error)
}

// instrumentStore reads the catalog rows behind a list of positions in one
// round trip; local so a fake can count trips and produce a missing row.
type instrumentStore interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

// spaceStore reads the space's base currency.
type spaceStore interface {
	SpaceByID(ctx context.Context, id uuid.UUID) (family.Space, error)
}

// Service values accounts and papers from their journals: positions, figures
// in the base currency, totals and the rules for sums that cannot be struck.
// The HTTP door (Handler) and the family total (cmd/babki) both read it.
type Service struct {
	ops         journalStore
	instruments instrumentStore
	quotes      quoteStore
	conv        marketdata.RateSource
	spaces      spaceStore
	accounts    accountStore
}

// accountStore reads whether an account's broker trades on foreign exchanges
// (decision Р-20); *account.Store satisfies it.
type accountStore interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (account.WithBalance, error)
}

func NewService(ops journalStore, instruments instrumentStore, quotes quoteStore, conv marketdata.RateSource, spaces spaceStore) *Service {
	return &Service{ops: ops, instruments: instruments, quotes: quotes, conv: conv, spaces: spaces}
}

// WithAccounts lets the valuation read an account's broker's reach (see
// tradesAbroad); without it every account is valued as a Russian broker's.
func (s *Service) WithAccounts(accounts accountStore) *Service {
	s.accounts = accounts
	return s
}

// tradesAbroad reports whether the account's broker trades on foreign
// exchanges. An account the store does not find is not one.
func (s *Service) tradesAbroad(ctx context.Context, spaceID, accountID uuid.UUID) (bool, error) {
	if s.accounts == nil {
		return false, nil
	}
	a, err := s.accounts.ByID(ctx, spaceID, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return a.TradesAbroad, err
}

// journalDoesNotCompute is the engine refusing a stored journal.
type journalDoesNotCompute struct{ err error }

func (e journalDoesNotCompute) Error() string { return e.err.Error() }
func (e journalDoesNotCompute) Unwrap() error { return e.err }

// errInstrumentNotInCatalog: the journal names a paper the catalog lacks —
// unreachable behind the foreign key, answered rather than skipped.
var errInstrumentNotInCatalog = errors.New("portfolio: the journal names an instrument the catalog does not hold")

// Positions is everything the positions screen shows for an account, and its
// operation count. It is the one place an account is valued; the
// family total reads it too (see ValueFromJournal).
func (s *Service) Positions(ctx context.Context, spaceID, accountID uuid.UUID) (apitypes.PositionsResponse, int, error) {
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}

	ops, err := s.ops.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}

	positions, err := Compute(ops)
	if err != nil {
		// Practically unreachable: writes replay the journal before committing.
		return apitypes.PositionsResponse{}, 0, journalDoesNotCompute{err}
	}

	instrumentIDs := make([]uuid.UUID, 0, len(positions))
	for id := range positions {
		instrumentIDs = append(instrumentIDs, id)
	}
	// One round trip for the catalog rows and one for the quotes.
	instruments, err := s.instruments.ByIDs(ctx, instrumentIDs)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}
	// One "today" for the whole request, shared by valuations, their conversions
	// and the prefetch.
	now := time.Now().UTC()
	abroad, err := s.tradesAbroad(ctx, spaceID, accountID)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}
	book, err := s.pricesOn(ctx, instrumentIDs, now, sp.FullValuation, todayWindows, abroad)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}
	quotes := book.fullQuotes()

	// Both per request.
	rates := marketdata.NewRateMemo(s.conv)
	income := incomeByInstrument(ops)

	// The account's money, folded from the same journal before the warm-up so its
	// rates join the same batch.
	cashPositions, err := Cash(ops)
	if err != nil {
		return apitypes.PositionsResponse{}, 0, err
	}
	rates.Prefetch(ctx, rateQueries(positions, instruments, quotes, income, cashPositions, sp.BaseCurrency, now))

	totals := newRealizedTotals(sp.BaseCurrency)
	account := newAccountTotals(sp.BaseCurrency)
	out := make([]apitypes.Position, 0, len(positions))
	for _, pos := range positions {
		inst, ok := instruments[pos.InstrumentID]
		if !ok {
			// A missing catalog row is unreachable behind the foreign key; answered with a
			// 404 rather than skipped, which would silently shrink every total.
			return apitypes.PositionsResponse{}, 0, errInstrumentNotInCatalog
		}
		// Russia's external loan bonds count their rouble cost at the day of the
		// sale (decision Р-21), before any figure is built from the position.
		atSaleRate := false
		if sp.TaxResidency == "RU" && inst.Type == instrument.TypeBond && instrument.RussianExternalBond(inst.ISIN) &&
			inst.FaceCurrency != nil {
			if atSaleRate, err = s.restateAtSaleRate(ctx, pos, *inst.FaceCurrency, sp.BaseCurrency, now, rates); err != nil {
				return apitypes.PositionsResponse{}, 0, err
			}
		}
		apiPos, err := s.toAPI(ctx, pos, inst, quotes, now, rates)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
		apiPos.CostAtSaleRate = atSaleRate
		if err := s.addLiquid(ctx, &apiPos, pos, inst, book, sp.BaseCurrency, now, rates); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}

		// Struck once, used twice: the position's in_base figure and a term of the
		// account total, which takes it even when the in_base object is absent.
		realizedMinor, realizedGap, err := s.realizedInBase(ctx, pos, sp.BaseCurrency, rates)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
		if err := totals.add(apiPos.Currency, apiPos.RealizedPnlMinor, realizedMinor, realizedGap, soldUnknownCost(pos)); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}

		inBase, gap, err := s.positionInBase(ctx, pos, apiPos, income[pos.InstrumentID], sp.BaseCurrency, realizedMinor, now, rates)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
		// The gap decides whether there is an object: a cause is published only
		// without one. If both ever came back, the object is dropped and the honest
		// caption kept.
		if apiGap, missing := apiInBaseGap(gap); missing {
			apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
			apiPos.InBaseGap = nullable.NewNullableWithValue(apiGap)
		} else {
			apiPos.InBaseGap = nullable.NewNullNullable[apitypes.InBaseGap]()
			// No gap: the object was struck, or the position is in the base currency;
			// the pointer tells them apart.
			if inBase != nil {
				apiPos.InBase = nullable.NewNullableWithValue(*inBase)
			} else {
				apiPos.InBase = nullable.NewNullNullable[apitypes.PositionInBase]()
			}
		}

		// Added after the in_base object is decided, since the total reads it.
		if err := account.addPosition(apiPos, inBase, gap, realizedGap); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}

		out = append(out, apiPos)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instrument.Name < out[j].Instrument.Name })

	// The account's money, valued here where the rates are.
	cash := make([]apitypes.CashPosition, 0, len(cashPositions))
	for _, p := range CashByCurrency(cashPositions) {
		one, err := s.cashToAPI(ctx, p, sp.BaseCurrency, now, rates)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
		cash = append(cash, one)
		if err := account.addCash(one); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
	}

	// The account's own charges, each at its own day's rate (НК РФ ст. 210 п. 5
	// for tax, consistency for the rest).
	for _, o := range accountCharges(ops) {
		minor, err := money.Sub(o.AmountMinor, o.FeeMinor)
		if err != nil {
			return apitypes.PositionsResponse{}, 0, fmt.Errorf("%w: a charge of %d less its own fee of %d", err, o.AmountMinor, o.FeeMinor)
		}
		base := nullable.NewNullableWithValue(minor)
		if o.Currency != sp.BaseCurrency {
			converted, ok, err := s.sumInBase(ctx, []datedMinor{{minor: minor, from: o.Currency, on: o.OccurredOn}}, sp.BaseCurrency, rates)
			if err != nil {
				return apitypes.PositionsResponse{}, 0, err
			}
			if !ok {
				base = nullable.NewNullNullable[int64]()
			} else {
				base = nullable.NewNullableWithValue(converted)
			}
		}
		if err := account.addCharge(o.Currency, minor, base); err != nil {
			return apitypes.PositionsResponse{}, 0, err
		}
	}

	// Every figure is FIFO within this account; whether that is the owner's
	// country's rule ships with the numbers, once per response.
	return apitypes.PositionsResponse{
		Positions:      out,
		CostBasisRules: family.CostBasisRulesAPI(sp.CostBasisRules()),
		// See realizedTotals and RealizedTotal in the contract.
		RealizedTotal: withAccountTax(totals.result(), ops),
		AccountTotal:  account.result(),
		Cash:          cash,
	}, len(ops), nil
}
