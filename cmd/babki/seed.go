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
			"Freedom KZ", account.TypeBrokerage, "USD", "Freedom Finance", false,
			[3]int64{8_200_00, 8_350_00, 8_500_00},
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
			OccurredOn: d("2026-05-06"), AmountMinor: 1_000_000, Currency: "USD",
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
		{
			AccountID: tbank, InstrumentID: inst("OFZ26238"), Type: operation.TypeCoupon,
			OccurredOn: d("2026-06-18"), AmountMinor: 354_000, Currency: "RUB",
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

	if err := seedMarketData(ctx, pool, instIDs, d); err != nil {
		return err
	}
	return nil
}

// seedMarketData records the FX rates and instrument quotes the demo
// space's market valuation depends on: GET /summary's total_in_base_minor
// (base currency RUB, so USD/KZT need a rate to convert) and GET
// .../positions' market_value_minor (share/bond positions need a quote to
// price). Everything is pinned to 2026-07-20 — the same date as the latest
// seeded account balance — so both features have live data to show on the
// same "as of" date the rest of the demo data uses.
//
// FXUS and AAPL deliberately get no quote: FXUS is Frozen, so its position
// demonstrates the null-valuation path (a holding the app can't safely
// price rather than a silent zero); AAPL is a live foreign instrument with
// no provider yet (cbr/moex only cover the Russian market — plan 4b widens
// this).
func seedMarketData(ctx context.Context, pool *pgxpool.Pool, instIDs map[string]uuid.UUID, d func(string) time.Time) error {
	mdStore := marketdata.NewStore(pool)
	on := d("2026-07-20")
	rate := decimal.RequireFromString

	rates := []marketdata.FxRate{
		{Base: "USD", Quote: "RUB", On: on, Rate: rate("78.50"), Source: "seed"},
		{Base: "EUR", Quote: "RUB", On: on, Rate: rate("92.30"), Source: "seed"},
		{Base: "KZT", Quote: "RUB", On: on, Rate: rate("0.163"), Source: "seed"},
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
	}
	if err := mdStore.UpsertQuotes(ctx, quotes); err != nil {
		return fmt.Errorf("seed quotes: %w", err)
	}
	return nil
}
