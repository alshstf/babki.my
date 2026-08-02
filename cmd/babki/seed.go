package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
)

func newSeedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "seed",
		Short: "Наполнить пустой инстанс демо-данными (демо-семья и счета)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalCtx()
			defer stop()
			r, err := setup(ctx, true)
			if err != nil {
				return err
			}
			defer r.close()
			if err := seedDemo(ctx, r.pool); err != nil {
				return err
			}
			r.log.Info("demo data seeded", "login", "demo", "password", "demo1234")
			return nil
		},
	}
}

// seedDemo populates an empty instance with a demo family and accounts.
func seedDemo(ctx context.Context, pool *pgxpool.Pool) error {
	famStore := family.NewStore(pool)
	svc := family.NewService(famStore)

	needed, err := svc.SetupNeeded(ctx)
	if err != nil {
		return err
	}
	if !needed {
		return fmt.Errorf("instance already has users; seed works only on an empty instance")
	}

	_, owner, err := svc.Setup(ctx, family.SetupParams{
		SpaceName: "Демо-семья", Username: "demo", DisplayName: "Александр", Password: "demo1234",
	})
	if err != nil {
		return err
	}
	if _, err := svc.CreateMember(ctx, owner, "partner", "Партнёр", "demo1234", family.RoleEditor); err != nil {
		return err
	}

	accStore := account.NewStore(pool)
	d := func(s string) time.Time {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			panic(err)
		}
		return t
	}
	dates := []time.Time{d("2026-05-31"), d("2026-06-30"), d("2026-07-20")}

	type seedAcc struct {
		name        string
		typ         account.Type
		currency    string
		institution string
		personal    bool
		balances    [3]int64 // minor units per date
	}
	seeds := []seedAcc{
		{
			"Брокерский Т-Банк", account.TypeBrokerage, "RUB", "Т-Банк", false,
			[3]int64{1_250_000_00, 1_310_000_00, 1_385_000_00},
		},
		{
			// The recorded balance of a brokerage account is taken to already
			// include the securities sitting in it (see the portfolio package
			// doc), so it has to exceed what the seeded positions cost: AAPL
			// ($6 209,20), MSFT ($10 000,00), the transferred TSLA ($1 900,00)
			// and what is left of NVDA ($1 500,00) run to $19 609,20 here. A
			// balance below that would put a single position above the whole
			// account it lives in, right on the screen this data exists to show.
			"Freedom KZ", account.TypeBrokerage, "USD", "Freedom Finance", false,
			[3]int64{24_000_00, 24_500_00, 25_000_00},
		},
		{
			"Текущий Сбер", account.TypeChecking, "RUB", "Сбер", false,
			[3]int64{145_000_00, 210_000_00, 180_000_00},
		},
		{
			"Вклад ГПБ", account.TypeDeposit, "RUB", "Газпромбанк", false,
			[3]int64{500_000_00, 500_000_00, 500_000_00},
		},
		{
			"Кредитка Альфа", account.TypeCreditCard, "RUB", "Альфа-Банк", false,
			[3]int64{-92_000_00, -45_000_00, -61_500_00},
		},
		{
			"Наличные", account.TypeCash, "RUB", "", true,
			[3]int64{70_000_00, 70_000_00, 70_000_00},
		},
	}
	accIDs := make(map[string]uuid.UUID, len(seeds))
	for _, s := range seeds {
		var personalOwner *uuid.UUID
		if s.personal {
			personalOwner = &owner.UserID
		}
		a, err := accStore.Create(ctx, owner.SpaceID, personalOwner, s.name, s.typ, s.currency, s.institution)
		if err != nil {
			return fmt.Errorf("seed account %q: %w", s.name, err)
		}
		accIDs[s.name] = a.ID
		for i, date := range dates {
			if err := accStore.SetBalance(ctx, owner.SpaceID, a.ID, date, s.balances[i]); err != nil {
				return fmt.Errorf("seed balance %q %s: %w", s.name, date.Format("2006-01-02"), err)
			}
		}
	}

	if err := seedInstrumentsAndOperations(ctx, pool, owner.SpaceID, accIDs, d); err != nil {
		return err
	}
	return nil
}

// seedInstrumentsAndOperations populates the global instrument catalog and
// records a chronological journal of operations against the two brokerage
// accounts, giving the demo space live positions with realized P&L and
// income — material the account/position UI (plan 3b) can render. All
// operations go through operation.Service so seed data is subject to the
// same validation and journal-consistency checks as user-entered data.
func seedInstrumentsAndOperations(
	ctx context.Context, pool *pgxpool.Pool, spaceID uuid.UUID,
	accIDs map[string]uuid.UUID, d func(string) time.Time,
) error {
	instStore := instrument.NewStore(pool)

	faceValue := int64(1_000_00)
	faceCurrency := "RUB"
	instSeeds := []struct {
		key  string
		inst instrument.Instrument
	}{
		{"SBER", instrument.Instrument{Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB"}},
		{"LKOH", instrument.Instrument{Type: instrument.TypeShare, Name: "Лукойл", Ticker: "LKOH", Currency: "RUB"}},
		{"OFZ26238", instrument.Instrument{
			Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "OFZ26238", Currency: "RUB",
			FaceValueMinor: &faceValue, FaceCurrency: &faceCurrency,
		}},
		{"FXUS", instrument.Instrument{Type: instrument.TypeETF, Name: "FinEx FXUS", Ticker: "FXUS", Currency: "USD", Frozen: true}},
		{"AAPL", instrument.Instrument{Type: instrument.TypeShare, Name: "Apple Inc.", Ticker: "AAPL", Currency: "USD"}},
		{"MSFT", instrument.Instrument{Type: instrument.TypeShare, Name: "Microsoft", Ticker: "MSFT", Currency: "USD"}},
		{"TSLA", instrument.Instrument{Type: instrument.TypeShare, Name: "Tesla", Ticker: "TSLA", Currency: "USD"}},
		{"NVDA", instrument.Instrument{Type: instrument.TypeShare, Name: "NVIDIA", Ticker: "NVDA", Currency: "USD"}},
	}
	instIDs := make(map[string]uuid.UUID, len(instSeeds))
	for _, is := range instSeeds {
		created, err := instStore.Create(ctx, is.inst)
		if err != nil {
			return fmt.Errorf("seed instrument %q: %w", is.key, err)
		}
		instIDs[is.key] = created.ID
	}

	inst := func(key string) *uuid.UUID {
		id := instIDs[key]
		return &id
	}
	qty := func(s string) *decimal.Decimal {
		v := decimal.RequireFromString(s)
		return &v
	}
	price := func(s string) *decimal.Decimal {
		v := decimal.RequireFromString(s)
		return &v
	}

	tbank := accIDs["Брокерский Т-Банк"]
	freedom := accIDs["Freedom KZ"]

	// Chronological order matters: operation.Service.Create replays each
	// account's journal through the portfolio engine and rejects entries
	// that would make it inconsistent (e.g. a sell recorded before its buy).
	ops := []operation.Operation{
		{
			AccountID: tbank, Type: operation.TypeDeposit,
			OccurredOn: d("2026-05-05"), AmountMinor: 1_500_000_00, Currency: "RUB",
		},
		{
			AccountID: freedom, Type: operation.TypeDeposit,
			OccurredOn: d("2026-05-06"), AmountMinor: 2_500_000, Currency: "USD",
		},
		// Apple is bought TWICE, and this first buy is deliberately dated
		// inside the gap before the fx history starts (see seededUSDRates):
		// one of this position's two lots has no rate on its own day, so the
		// whole position honestly refuses to be expressed in rubles instead
		// of publishing a basis summed from only the lot that did convert.
		// On screen the row therefore stays in dollars with the "not
		// converted" marker (displayCurrency.notConverted) even in
		// base-currency mode — the position-level twin of what the same gap
		// does to a single journal entry (the deposit above).
		{
			AccountID: freedom, InstrumentID: inst("AAPL"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-08"), Quantity: qty("10"), Price: price("200"),
			AmountMinor: -200_000, FeeMinor: 200, Currency: "USD",
		},
		{
			AccountID: tbank, InstrumentID: inst("SBER"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-10"), Quantity: qty("300"), Price: price("305.5"),
			AmountMinor: -9_165_000, FeeMinor: 9_165, Currency: "RUB",
		},
		{
			AccountID: tbank, InstrumentID: inst("OFZ26238"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-12"), Quantity: qty("100"), Price: price("950"),
			AmountMinor: -9_500_000, FeeMinor: 9_500, Currency: "RUB",
		},
		// TSLA is bought entirely at Т-Банк, in two lots on two dates with two
		// different fx rates, and later transferred whole to Freedom KZ (see
		// the transfer call below) — the seed's demonstration of plan 7a: a
		// transfer between the family's own accounts must carry each lot's
		// OWN purchase date into the receiving account, not collapse
		// everything to the day of the move. Both rates (seededUSDRates:
		// 60.00 on 2026-05-13, 64.00 on 2026-06-15) are deliberately BELOW
		// every other rate in the table, including the transfer day's own
		// (78.50) — so a collapse-to-transfer-date bug cannot partially
		// cancel against a lot that happens to already share that rate (the
		// way it does, more subtly, for MSFT below) and instead overvalues
		// the position by a wide, unmistakable margin. See the transfer call
		// for the full arithmetic.
		{
			AccountID: tbank, InstrumentID: inst("TSLA"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-13"), Quantity: qty("5"), Price: price("180"),
			AmountMinor: -90_000, Currency: "USD",
		},
		// NVDA is plan 7c's demonstration and needs THREE parcels' worth of
		// history to make its point: this one, bought at Т-Банк and later
		// transferred, is the EARLIEST acquisition of the three, and the
		// account it is transferred INTO already holds a later one (see the
		// 2026-06-20 buy below). When part of the holding is sold there, this
		// parcel is the one that leaves — because the queue is ordered by the
		// day shares were bought, not by the day they arrived. See the
		// transfer call for the whole arithmetic.
		{
			AccountID: tbank, InstrumentID: inst("NVDA"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-14"), Quantity: qty("10"), Price: price("100"),
			AmountMinor: -100_000, Currency: "USD",
		},
		{
			AccountID: tbank, InstrumentID: inst("FXUS"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-20"), Quantity: qty("30"), Price: price("85"),
			AmountMinor: -255_000, Currency: "USD",
		},
		{
			AccountID: tbank, InstrumentID: inst("LKOH"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-03"), Quantity: qty("20"), Price: price("7300"),
			AmountMinor: -14_600_000, FeeMinor: 14_600, Currency: "RUB",
		},
		{
			AccountID: freedom, InstrumentID: inst("AAPL"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-10"), Quantity: qty("20"), Price: price("210.15"),
			AmountMinor: -420_300, FeeMinor: 420, Currency: "USD",
		},
		// The demo's one position whose profit has a DIFFERENT SIGN in its own
		// currency and in rubles — the consequence of the owner's decision
		// (2026-07-29) that ruble return includes the currency's own move
		// while position-currency return does not. Without it that decision
		// is invisible on demo data and only a unit test knows about it.
		//
		// Bought on a day the ruble was weak and valued today at a stronger
		// ruble, so a gain in dollars is a loss in rubles. Every figure is
		// meant to be checkable by eye, which is also why this buy carries no
		// broker fee: cost basis is exactly the amount paid.
		//
		//	cost    20 × $500.00 = $10 000.00 =   1_000_000 minor USD
		//	  in ₽  1_000_000 × 81.40 (2026-06-10, the LOT'S OWN day) = 81_400_000 = 814 000,00 ₽
		//	value   20 × $510.00 = $10 200.00 =   1_020_000 minor USD
		//	  in ₽  1_020_000 × 78.50 (today's rate, i.e. 2026-07-20's) = 80_070_000 = 800 700,00 ₽
		//
		//	profit in USD =  1_020_000 −  1_000_000 =    +20_000  (+$200.00, +2.0 %)
		//	profit in RUB = 80_070_000 − 81_400_000 = −1_330_000  (−13 300,00 ₽, −1.6 %)
		//
		// The pre-plan-6 semantics converted the whole basis at today's rate —
		// 1_000_000 × 78.50 = 78_500_000 — and so reported a ruble PROFIT of
		// 80_070_000 − 78_500_000 = +1_570_000 (+15 700,00 ₽): the dollar
		// profit times a rate, with the currency's move cancelled out of it.
		// The two numbers differ in sign, which is the whole point.
		//
		// This depends on two seeded facts: 2026-06-10 has a rate of its own
		// (81.40) and 2026-07-20 is the newest rate in the table, so it is the
		// one "today" resolves to. On an instance that reaches cbr.ru the
		// backfill job replaces both with reality and the demo's arithmetic
		// stops being the arithmetic above — the same caveat the fx gap below
		// already carries.
		{
			AccountID: freedom, InstrumentID: inst("MSFT"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-10"), Quantity: qty("20"), Price: price("500"),
			AmountMinor: -1_000_000, Currency: "USD",
		},
		// TSLA's second lot — see the first lot above for why the rates and
		// the transfer are shaped the way they are.
		{
			AccountID: tbank, InstrumentID: inst("TSLA"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-15"), Quantity: qty("5"), Price: price("200"),
			AmountMinor: -100_000, Currency: "USD",
		},
		{
			AccountID: tbank, InstrumentID: inst("OFZ26238"), Type: operation.TypeCoupon,
			OccurredOn: d("2026-06-18"), AmountMinor: 354_000, Currency: "RUB",
		},
		// NVDA's second parcel, bought at Freedom KZ itself — a month AFTER
		// the one at Т-Банк and a month BEFORE that one is transferred here.
		// It is therefore first by arrival and second by acquisition, which is
		// the whole disagreement plan 7c settles: the sale below consumes the
		// transferred parcel and leaves this one.
		{
			AccountID: freedom, InstrumentID: inst("NVDA"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-20"), Quantity: qty("10"), Price: price("150"),
			AmountMinor: -150_000, Currency: "USD",
		},
		// A foreign-currency operation on a RUB account, deliberately dated a
		// Saturday: the CBR publishes no rate on weekends, so the journal has
		// to convert it at Friday's rate and say so. Together with the FXUS
		// buy above it also gives this account's journal two USD entries on
		// two different dates — the display-currency toggle only appears when
		// a screen actually shows more than one currency, and two dates are
		// what make "converted at the rate of its own day" visible on screen
		// rather than merely true in the database.
		{
			AccountID: tbank, Type: operation.TypeDeposit,
			OccurredOn: d("2026-07-04"), AmountMinor: 80_000, Currency: "USD",
		},
		{
			AccountID: tbank, InstrumentID: inst("SBER"), Type: operation.TypeDividend,
			OccurredOn: d("2026-07-08"), AmountMinor: 1_045_200, Currency: "RUB",
		},
		{
			AccountID: tbank, InstrumentID: inst("SBER"), Type: operation.TypeTax,
			OccurredOn: d("2026-07-08"), AmountMinor: -135_876, Currency: "RUB",
		},
		{
			AccountID: tbank, InstrumentID: inst("LKOH"), Type: operation.TypeSell,
			OccurredOn: d("2026-07-15"), Quantity: qty("5"), Price: price("7550"),
			AmountMinor: 3_775_000, FeeMinor: 3_775, Currency: "RUB",
		},
	}

	opSvc := operation.NewService(operation.NewStore(pool))
	for _, op := range ops {
		if _, err := opSvc.Create(ctx, spaceID, op); err != nil {
			return fmt.Errorf("seed operation %s %s: %w", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	// The transfer plan 7a exists for: all 10 TSLA shares move from Т-Банк to
	// Freedom KZ on 2026-07-20 — the same date every other "today" figure in
	// this seed is struck on (the newest row in seededUSDRates). This must go
	// through CreateTransfer, not Service.Create (which rejects
	// transfer_in/transfer_out directly — see validate): it is CreateTransfer
	// that resolves the source FIFO lots and carries their real acquisition
	// dates onto the receiving leg (operation.Operation.TransferLots).
	//
	// Manual arithmetic, every step exact:
	//
	//	lot 1: 5 @ $180.00 on 2026-05-13 -> 90_000 minor USD
	//	  at its OWN day's rate (60.00): 90_000 * 60.00 =  5_400_000 =  54 000,00 ₽
	//	lot 2: 5 @ $200.00 on 2026-06-15 -> 100_000 minor USD
	//	  at its OWN day's rate (64.00): 100_000 * 64.00 =  6_400_000 =  64 000,00 ₽
	//
	//	destination cost_minor (USD)            =  90_000 + 100_000        =    190_000 (= $1 900.00)
	//	in_base.cost_minor (correct, per lot)   = 5_400_000 + 6_400_000    = 11_800_000 (= 118 000,00 ₽)
	//
	//	what a transfer that collapsed both lots into the TRANSFER day instead
	//	(2026-07-20, rate 78.50) would report:
	//	  190_000 * 78.50 = 14_915_000 = 149 150,00 ₽
	//	  — 31 150,00 ₽ (≈26 %) more than the truth, invented purely by
	//	  re-dating shares that did not change value the day they changed
	//	  brokers. Both figures are meant to be checked by eye against a real
	//	  GET .../positions response on the destination account.
	//
	// BOTH JOURNAL ROWS read 118 000,00 ₽ as well — the departure from Т-Банк
	// and the arrival at Freedom KZ: a transfer's amount is a basis assembled
	// on other days, so the journal converts it piece by piece at those days'
	// rates, exactly as the position does (see
	// operation.Handler.operationInBase). Each fix left the invented
	// 149 150,00 ₽ standing on one screen fewer — first next to a position
	// saying 118 000,00 ₽, then on the source account's journal alone, one pair
	// disagreeing with itself about the same ten shares. It is now nowhere in
	// the demo, and the arithmetic below is the only place it appears at all.
	if _, _, err := opSvc.CreateTransfer(ctx, spaceID, operation.TransferParams{
		FromAccountID: tbank, ToAccountID: freedom, InstrumentID: instIDs["TSLA"],
		Quantity: decimal.RequireFromString("10"), OccurredOn: d("2026-07-20"),
	}); err != nil {
		return fmt.Errorf("seed transfer TSLA: %w", err)
	}

	// The transfer plan 7c exists for. All 10 NVDA move from Т-Банк to Freedom
	// KZ on the same day TSLA does — but unlike TSLA, the destination ALREADY
	// HOLDS a parcel of this instrument, bought there later than the one
	// arriving. Two days after, half the holding is sold, and which parcel that
	// sale consumes is the whole question.
	//
	// Manual arithmetic, every step exact:
	//
	//	arriving parcel: 10 @ $100.00 bought 2026-05-14 at Т-Банк  -> 100_000 minor USD
	//	parcel already here: 10 @ $150.00 bought 2026-06-20        -> 150_000
	//	sale: 10 @ $200.00 on 2026-07-22                           -> 200_000
	//
	//	BY ACQUISITION (what this application does): the 2026-05-14 parcel is
	//	the earliest purchase anywhere in this account, so it leaves first —
	//	moving shares between the family's own accounts is not a purchase and
	//	cannot decide what is sold (НК РФ ст. 214.1 п. 13 «первых по времени
	//	приобретений»; 26 CFR 1.1012-1(c)(1)(i) "the earliest lot the taxpayer
	//	purchased or acquired").
	//	  realized P&L  = 200_000 − 100_000 = +100_000 (+$1 000.00)
	//	  cost_minor    = 150_000 ($1 500.00), one lot, dated 2026-06-20
	//	  in_base       = 150_000 × 65.00 (that lot's OWN day) = 9_750_000 (97 500,00 ₽)
	//
	//	BY ARRIVAL (what it did before plan 7c — the parcel already sitting
	//	here would have gone first):
	//	  realized P&L  = 200_000 − 150_000 =  +50_000 (+$500.00) — HALF
	//	  cost_minor    = 100_000 ($1 000.00), dated 2026-05-14
	//	  in_base       = 100_000 × 60.50 = 6_050_000 (60 500,00 ₽) — 37 000,00 ₽ less
	//
	// The numbers are deliberately round and far apart: the profit doubles and
	// the remaining ruble basis moves by more than a third, so the difference
	// is visible by eye on the account screen rather than only in a test.
	if _, _, err := opSvc.CreateTransfer(ctx, spaceID, operation.TransferParams{
		FromAccountID: tbank, ToAccountID: freedom, InstrumentID: instIDs["NVDA"],
		Quantity: decimal.RequireFromString("10"), OccurredOn: d("2026-07-20"),
	}); err != nil {
		return fmt.Errorf("seed transfer NVDA: %w", err)
	}

	// Recorded after the transfer on purpose: Service.Create replays the
	// journal, and the parcel this sale is meant to consume only exists in
	// this account once the transfer above has been written.
	if _, err := opSvc.Create(ctx, spaceID, operation.Operation{
		AccountID: freedom, InstrumentID: inst("NVDA"), Type: operation.TypeSell,
		OccurredOn: d("2026-07-22"), Quantity: qty("10"), Price: price("200"),
		AmountMinor: 200_000, Currency: "USD",
	}); err != nil {
		return fmt.Errorf("seed operation sell NVDA: %w", err)
	}

	if err := seedMarketData(ctx, pool, instIDs, d); err != nil {
		return err
	}
	return nil
}

// seededUSDRates is the demo instance's USD/RUB history, one plausible rate
// per date. Each date is there for a reason, and the set as a whole is what
// the demo screens show:
//
//   - 2026-05-13 and 2026-06-15 — the two TSLA buys' own dates (see the TSLA
//     lots in seedInstrumentsAndOperations): TSLA is later transferred whole
//     from Т-Банк to Freedom KZ, and this pair is the demo's example of plan
//     7a — a transfer carries each lot's own acquisition date into the
//     receiving account instead of collapsing the basis onto the transfer
//     day. Both rates (60.00 and 64.00) sit BELOW everything else in this
//     table, including the transfer day's own rate (78.50), and in the SAME
//     direction, so a collapse-to-transfer-date bug cannot partially cancel
//     against a lot that happens to already share that rate (the way it does,
//     more subtly, for MSFT below) — it overvalues the position by a wide,
//     unmistakable margin instead (see the transfer call for the arithmetic).
//   - 2026-05-14 and 2026-06-20 — the two NVDA buys' own dates, and plan 7c's
//     pair: the first is the parcel bought at Т-Банк and later transferred to
//     Freedom KZ, the second the parcel bought at Freedom KZ itself in
//     between. A sale there consumes the EARLIER one even though it arrived
//     last, and the two rates (60.50 and 65.00) are what make that visible in
//     rubles as well as in dollars — the surviving parcel's ruble basis is
//     97 500,00 ₽ where arrival order would have left 60 500,00 ₽. See the
//     NVDA transfer call for the full arithmetic.
//   - 2026-05-20 — the FXUS buy's own date: converted at the exact date's
//     rate, the ordinary case.
//   - 2026-06-10 — the AAPL and MSFT buys' own date, at a visibly different
//     rate: two operations, two dates, two rates, so the journal cannot be
//     mistaken for "everything at today's rate". It is also the highest rate
//     in the set, and deliberately so: it is the day the MSFT lot was bought,
//     and a basis struck at 81.40 against a valuation struck at 78.50 is what
//     turns that position's dollar profit into a ruble loss (see the MSFT buy).
//   - 2026-07-03 — the Friday before the Saturday USD deposit, which has no
//     rate of its own: the entry converts at the nearest earlier date and
//     the journal discloses that date rather than claiming the operation's
//     own.
//   - 2026-07-20 — today's-rate anchor, shared with the quotes and the
//     latest account balances; also what GET /summary converts at, and the
//     TSLA transfer's own date.
//
// Nothing is seeded on or before 2026-05-08, the dates of the demo's two
// earliest USD operations (the Freedom KZ deposit and the first AAPL buy),
// and that gap is deliberate. It buys two demonstrations at once:
//
//   - the deposit has no resolvable rate at all, so that journal entry must
//     show its original amount with an explanation instead of a dash or a
//     zero;
//   - the AAPL buy is one of two lots of a live position, so that whole
//     position must refuse to be shown in rubles rather than publish a basis
//     built from the one lot that did convert.
//
// Seeding a rate for either would hide the honest-degradation paths behind
// unit tests where nobody looks at them. On an instance with internet access
// the fx backfill job eventually fills those dates from cbr.ru and both start
// converting on their own — which is the point of the job.
var seededUSDRates = []struct{ on, rate string }{
	{"2026-05-13", "60.00"},
	{"2026-05-14", "60.50"},
	{"2026-05-20", "79.15"},
	{"2026-06-10", "81.40"},
	{"2026-06-15", "64.00"},
	{"2026-06-20", "65.00"},
	{"2026-07-03", "77.90"},
	{"2026-07-20", "78.50"},
}

// seedMarketData records the FX rates and instrument quotes the demo
// space's market valuation depends on: GET /summary's total_in_base_minor
// (base currency RUB, so USD/KZT need a rate to convert), GET
// .../positions' market_value_minor (share/bond positions need a quote to
// price), and the journal's per-operation in_base (each foreign-currency
// entry converted at the rate of the day it happened).
//
// Quotes and the EUR/KZT rates are pinned to 2026-07-20 — the same date as
// the latest seeded account balance — because both answer "what is this
// worth now". USD/RUB, in contrast, is seeded as a short HISTORY, with a
// deliberately different rate per date: the journal converts every entry at
// its own date's rate, and a single flat rate would make a historical
// conversion and a today's-rate conversion produce identical numbers,
// leaving the feature invisible on the demo screens. See seededUSDRates for
// what each date demonstrates.
//
// FXUS and AAPL deliberately get no quote: FXUS is Frozen, so its position
// demonstrates the null-valuation path (a holding the app can't safely
// price rather than a silent zero); AAPL is a live foreign instrument with
// no provider yet (cbr/moex only cover the Russian market — plan 4b widens
// this).
//
// MSFT is the exception among the foreign instruments, and it is hand-seeded
// rather than provider-supplied for one reason: a position needs a valuation
// before its profit can differ in sign between its own currency and rubles,
// and that difference is the demo's whole reason for existing (see the MSFT
// buy for the arithmetic). 510.00 against a 500.00 purchase is a 2 % gain in
// dollars — small on purpose, because the ruble moved 3.7 % the other way
// over the same weeks and the point is that the smaller move loses.
func seedMarketData(ctx context.Context, pool *pgxpool.Pool, instIDs map[string]uuid.UUID, d func(string) time.Time) error {
	mdStore := marketdata.NewStore(pool)
	on := d("2026-07-20")
	rate := decimal.RequireFromString

	rates := []marketdata.FxRate{
		{Base: "EUR", Quote: "RUB", On: on, Rate: rate("92.30"), Source: "seed"},
		{Base: "KZT", Quote: "RUB", On: on, Rate: rate("0.163"), Source: "seed"},
	}
	for _, r := range seededUSDRates {
		rates = append(rates, marketdata.FxRate{
			Base: "USD", Quote: "RUB", On: d(r.on), Rate: rate(r.rate), Source: "seed",
		})
	}
	if err := mdStore.UpsertFxRates(ctx, rates); err != nil {
		return fmt.Errorf("seed fx rates: %w", err)
	}

	// OFZ26238's price is a percentage of face value (95.20 meaning 95.20%),
	// same convention as a real bond quote — see portfolio.marketValue.
	quotes := []marketdata.Quote{
		{InstrumentID: instIDs["SBER"], On: on, Price: rate("305.50"), Currency: "RUB", Source: "seed"},
		{InstrumentID: instIDs["LKOH"], On: on, Price: rate("7550.00"), Currency: "RUB", Source: "seed"},
		{InstrumentID: instIDs["OFZ26238"], On: on, Price: rate("95.20"), Currency: "RUB", Source: "seed"},
		{InstrumentID: instIDs["MSFT"], On: on, Price: rate("510.00"), Currency: "USD", Source: "seed"},
		// NVDA is quoted at exactly the price the 2026-07-22 sale went off at,
		// so the surviving parcel is valued at $2 000.00 against a $1 500.00
		// basis: +$500.00 unrealized beside +$1 000.00 realized, both round and
		// both readable off the row without a calculator.
		{InstrumentID: instIDs["NVDA"], On: on, Price: rate("200.00"), Currency: "USD", Source: "seed"},
	}
	if err := mdStore.UpsertQuotes(ctx, quotes); err != nil {
		return fmt.Errorf("seed quotes: %w", err)
	}
	return nil
}
