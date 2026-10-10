package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/operation"
)

func newSeedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "seed",
		Short: "Наполнить пустой инстанс демо-данными (демо-семья и счета)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signalCtx(cmd.Context())
			defer stop()
			// The seed writes no encrypted secret, so it runs without
			// BABKI_ENCRYPTION_KEY.
			r, err := setup(ctx, true, false)
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

// seedDemo populates an empty instance with a demo family and accounts, in one
// transaction: once the owner exists the instance counts as set up, so a seed
// failing midway (a duplicate ticker, a refused operation, Ctrl-C) would leave a
// fragment that `babki seed` refuses to touch again. The stores are built on the
// transaction.
func seedDemo(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	// Rolled back on a context the interrupt cannot cancel; the error is
	// dropped (after Commit it is ErrTxClosed, and on failure the caller returns
	// the real error).
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := seedDemoTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func seedDemoTx(ctx context.Context, tx pgx.Tx) error {
	famStore := family.NewStore(tx)
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

	accStore := account.NewStore(tx)
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
			// A brokerage account's recorded balance includes its securities, so it
			// must exceed what the seeded positions cost:
			//
			// 	AAPL                       $6 209,20
			// 	MSFT                      $10 000,00
			// 	TSLA (transferred in)      $1 900,00
			// 	NVDA (what is left)        $1 500,00
			// 	KAZ32EUR (the eurobond)    $5 750,00
			// 	WEWKQ                      $2 000,00
			// 	INTC (hand-entered basis)  $3 000,00
			// 	AMZN (two transfers)       $3 800,00
			// 	                          $34 159,20
			//
			// Alphabet is absent: bought and sold in full, it survives only in the
			// account's «Зафиксировано» line.
			"Freedom KZ", account.TypeBrokerage, "USD", "Freedom Finance", false,
			[3]int64{35_000_00, 36_000_00, 37_000_00},
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

	if err := seedInstrumentsAndOperations(ctx, tx, owner.SpaceID, accIDs, d); err != nil {
		return err
	}
	if err := seedTinvestDemo(ctx, tx, owner.SpaceID, accIDs["Брокерский Т-Банк"], d); err != nil {
		return err
	}
	return seedHousehold(ctx, tx, owner.SpaceID, accIDs, d)
}

// seedTinvestDemo gives the demo a T-Invest connection with something on every
// importer screen: the connection, its broker account, a run log, a
// reconciliation difference and unparsed operations.
//
// It never touches the network: the connection is disabled (the dispatcher skips
// it, "sync now" refuses with 409) and its token is random bytes, not a sealed
// secret.
//
// Every run-log figure is what the two SyncMirror calls returned. Run times are
// fixed explicitly (stampSeedRun): inside one transaction now() is a single
// instant, and two identical stamps would order the runs by a random uuid.
func seedTinvestDemo(ctx context.Context, tx pgx.Tx, spaceID, accountID uuid.UUID,
	d func(string) time.Time,
) error {
	store := tinvest.NewStore(tx)

	// Random bytes, not a sealed token: the seed has no key, and a demo must
	// not hold a usable token.
	ciphertext := make([]byte, 64)
	if _, err := rand.Read(ciphertext); err != nil {
		return fmt.Errorf("seed tinvest connection: %w", err)
	}
	conn, err := store.CreateConnection(ctx, spaceID, ciphertext, "0000", tinvest.StatusDisabled)
	if err != nil {
		return fmt.Errorf("seed tinvest connection: %w", err)
	}
	openedOn := d("2019-03-14")
	link, err := store.CreateLink(ctx, tinvest.AccountLink{
		ConnectionID:      conn.ID,
		SpaceID:           spaceID,
		AccountID:         accountID,
		BrokerAccountID:   "2000123456",
		BrokerAccountName: "Брокерский счёт",
		BrokerAccountType: "ACCOUNT_TYPE_TINKOFF",
		OpenedOn:          &openedOn,
	})
	if err != nil {
		return fmt.Errorf("seed tinvest account link: %w", err)
	}

	// Three operations the projection has no rules for: variation margin and
	// an overnight repo (owner's decision 5: shown unparsed until there is live
	// data) and a currency purchase, a refusal of its own.
	first := seedTinvestOperations(d)
	// The two runs an hour apart; each instant is also what its sync stamps
	// on the mirror rows, so the log matches its rows.
	firstAt := d("2026-07-20").Add(9 * time.Hour)
	secondAt := d("2026-07-20").Add(10 * time.Hour)

	firstRun, err := store.StartRun(ctx, conn.ID, link.ID, tinvest.TriggerInitial)
	if err != nil {
		return fmt.Errorf("seed tinvest first run: %w", err)
	}
	firstStats, err := store.SyncMirror(ctx, conn.ID, link, first, firstAt)
	if err != nil {
		return fmt.Errorf("seed tinvest mirror: %w", err)
	}
	rows, err := store.MirrorRowsByLink(ctx, link.ID)
	if err != nil {
		return fmt.Errorf("seed tinvest mirror rows: %w", err)
	}
	verdicts := map[uuid.UUID]tinvest.UnparsedVerdict{}
	for _, row := range rows {
		verdicts[row.ID] = seedTinvestVerdicts[row.BrokerOperationID]
	}
	if err := store.SetUnparsedVerdicts(ctx, verdicts); err != nil {
		return fmt.Errorf("seed tinvest unparsed verdicts: %w", err)
	}
	if err := store.FinishRun(ctx, firstRun.ID, tinvest.RunOutcome{
		Status:           tinvest.RunOK,
		ReadCount:        firstStats.Read,
		AddedCount:       firstStats.Added,
		DisappearedCount: firstStats.Disappeared,
		UnparsedCount:    len(verdicts),
	}); err != nil {
		return fmt.Errorf("seed tinvest first run outcome: %w", err)
	}
	if err := stampSeedRun(ctx, tx, firstRun.ID, firstAt); err != nil {
		return err
	}

	// A second run in which the broker stopped returning the repo: the row is
	// marked, not deleted, and stays on the unparsed list.
	second := first[:len(first)-1]
	secondRun, err := store.StartRun(ctx, conn.ID, link.ID, tinvest.TriggerSchedule)
	if err != nil {
		return fmt.Errorf("seed tinvest second run: %w", err)
	}
	secondStats, err := store.SyncMirror(ctx, conn.ID, link, second, secondAt)
	if err != nil {
		return fmt.Errorf("seed tinvest mirror refresh: %w", err)
	}
	if err := store.FinishRun(ctx, secondRun.ID, tinvest.RunOutcome{
		Status:           tinvest.RunOK,
		ReadCount:        secondStats.Read,
		AddedCount:       secondStats.Added,
		DisappearedCount: secondStats.Disappeared,
		UnparsedCount:    len(verdicts),
		// One real difference: no futures position in the journal, because the
		// variation margin above never became an entry.
		Reconcile: tinvest.ReconcileResult{
			Status: tinvest.ReconcileMismatched,
			Mismatches: []tinvest.ReconcileMismatch{{
				Kind:    tinvest.MismatchUnsupported,
				Label:   "SiH6 (futures)",
				Broker:  decimal.NewFromInt(2),
				Journal: decimal.Zero,
			}},
		},
	}); err != nil {
		return err
	}
	return stampSeedRun(ctx, tx, secondRun.ID, secondAt)
}

// stampSeedRun sets a seeded run's started_at and finished_at to the instant
// its SyncMirror call stamped, replacing now(). Production has no use for this,
// so it is an UPDATE here rather than an argument to StartRun and FinishRun; it is
// the only code outside the importer writing run-log columns. A seeded run gets no
// invented duration. reconciled_at moves only where there is one: an unchecked
// run keeps NULL.
func stampSeedRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID, at time.Time) error {
	ct, err := tx.Exec(ctx, `UPDATE tinvest_sync_runs
		SET started_at = $2::timestamptz, finished_at = $2::timestamptz,
		    reconciled_at = CASE WHEN reconciled_at IS NULL THEN NULL ELSE $2::timestamptz END
		WHERE id = $1`, runID, at)
	if err != nil {
		return fmt.Errorf("seed tinvest run clock: %w", err)
	}
	if ct.RowsAffected() != 1 {
		return fmt.Errorf("seed tinvest run clock: run %s matched %d rows, want 1",
			runID, ct.RowsAffected())
	}
	return nil
}

// seedTinvestVerdicts is why each seeded operation did not become an entry,
// keyed by broker operation id, recorded after the mirror as
// Store.SetUnparsedVerdicts does. Each detail is the sentence ProjectRow writes
// for that shape, copied rather than invented; the two unsupported types share a
// code and differ in the operation named.
var seedTinvestVerdicts = map[string]tinvest.UnparsedVerdict{
	"seed-varmargin-1": {
		Reason: string(tinvest.ReasonUnsupportedType),
		Detail: `broker operation type "OPERATION_TYPE_WRITING_OFF_VARMARGIN"`,
	},
	"seed-fxbuy-1": {
		Reason: string(tinvest.ReasonCurrencyTrade),
		Detail: "a currency trade whose traded currency and nominal the broker would not say",
	},
	"seed-overnight-1": {
		Reason: string(tinvest.ReasonUnsupportedType),
		Detail: `broker operation type "OPERATION_TYPE_OVERNIGHT"`,
	},
}

// seedTinvestOperations is the broker's side of the demo import. Raw is
// written in full because the unparsed screen shows the broker's own document.
func seedTinvestOperations(d func(string) time.Time) []tinvest.OperationItem {
	return []tinvest.OperationItem{
		{
			ID:             "seed-varmargin-1",
			Type:           "OPERATION_TYPE_WRITING_OFF_VARMARGIN",
			State:          "OPERATION_STATE_EXECUTED",
			Date:           d("2026-06-18").Add(19 * time.Hour),
			InstrumentType: "futures",
			InstrumentUID:  "9654c2dd-6993-427e-80fa-04e80a1cf4da",
			FIGI:           "FUTSI0326000",
			Payment:        tinvest.MoneyValue{Currency: "RUB", Units: -1240, Nano: -500_000_000},
			Description:    "Списание вариационной маржи",
			Raw: json.RawMessage(`{"id":"seed-varmargin-1","type":"OPERATION_TYPE_WRITING_OFF_VARMARGIN",` +
				`"state":"OPERATION_STATE_EXECUTED","date":"2026-06-18T19:00:00Z",` +
				`"instrumentType":"futures","instrumentUid":"9654c2dd-6993-427e-80fa-04e80a1cf4da",` +
				`"figi":"FUTSI0326000","payment":{"currency":"rub","units":"-1240","nano":-500000000},` +
				`"quantity":"0","description":"Списание вариационной маржи"}`),
		},
		{
			ID:             "seed-fxbuy-1",
			Type:           "OPERATION_TYPE_BUY",
			State:          "OPERATION_STATE_EXECUTED",
			Date:           d("2026-07-02").Add(12 * time.Hour),
			InstrumentType: "currency",
			InstrumentUID:  "a22a1263-8e1b-4546-a1aa-416463f104d3",
			FIGI:           "BBG0013HGFT4",
			Payment:        tinvest.MoneyValue{Currency: "RUB", Units: -78_450, Nano: 0},
			Price:          tinvest.MoneyValue{Currency: "RUB", Units: 78, Nano: 450_000_000},
			Quantity:       1000,
			Description:    "Покупка USD/RUB",
			Raw: json.RawMessage(`{"id":"seed-fxbuy-1","type":"OPERATION_TYPE_BUY",` +
				`"state":"OPERATION_STATE_EXECUTED","date":"2026-07-02T12:00:00Z",` +
				`"instrumentType":"currency","instrumentUid":"a22a1263-8e1b-4546-a1aa-416463f104d3",` +
				`"figi":"BBG0013HGFT4","payment":{"currency":"rub","units":"-78450","nano":0},` +
				`"price":{"currency":"rub","units":"78","nano":450000000},"quantity":"1000",` +
				`"description":"Покупка USD/RUB"}`),
		},
		{
			ID:             "seed-overnight-1",
			Type:           "OPERATION_TYPE_OVERNIGHT",
			State:          "OPERATION_STATE_EXECUTED",
			Date:           d("2026-07-09").Add(21 * time.Hour),
			InstrumentType: "share",
			InstrumentUID:  "e6123145-9665-43e0-8413-cd61b8aa9b13",
			FIGI:           "BBG004730N88",
			Payment:        tinvest.MoneyValue{Currency: "RUB", Units: 31, Nano: 720_000_000},
			Description:    "Доход от предоставления бумаг овернайт",
			Raw: json.RawMessage(`{"id":"seed-overnight-1","type":"OPERATION_TYPE_OVERNIGHT",` +
				`"state":"OPERATION_STATE_EXECUTED","date":"2026-07-09T21:00:00Z",` +
				`"instrumentType":"share","instrumentUid":"e6123145-9665-43e0-8413-cd61b8aa9b13",` +
				`"figi":"BBG004730N88","payment":{"currency":"rub","units":"31","nano":720000000},` +
				`"quantity":"0","description":"Доход от предоставления бумаг овернайт"}`),
		},
	}
}

// seedInstrumentsAndOperations fills the catalog and records a chronological
// journal on the two brokerage accounts through operation.Service, so the demo
// data passes the same checks as user input.
func seedInstrumentsAndOperations(
	ctx context.Context, tx pgx.Tx, spaceID uuid.UUID,
	accIDs map[string]uuid.UUID, d func(string) time.Time,
) error {
	instStore := instrument.NewStore(tx)

	// The OFZ: a 1 000 ₽ face on a rouble bond, the case where the trade
	// dialog can turn a percentage of face into money per bond (see its buy).
	faceValue := int64(1_000_00)
	faceCurrency := "RUB"
	// The eurobond's face is in a third currency, neither the position's
	// dollars nor the space's roubles (#39). It is also the trade dialog's
	// refusal (#77): a percentage of a EUR face is euros, the trade is in
	// dollars, and the dialog has no rate, so the percent field is disabled
	// with a sentence naming why.
	eurFaceValue := int64(1_000_00)
	eurFaceCurrency := "EUR"
	instSeeds := []struct {
		key  string
		inst instrument.Instrument
	}{
		{"SBER", instrument.Instrument{Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB"}},
		{"LKOH", instrument.Instrument{Type: instrument.TypeShare, Name: "Лукойл", Ticker: "LKOH", Currency: "RUB"}},
		// The ticker is the exchange's SECID, "SU26238RMFS4", not "OFZ26238":
		// the quotes job asks MOEX for catalog tickers verbatim, and MOEX answers by
		// SECID (moex.parseSecurities), so anything else is never priced (#35).
		// Checked on iss.moex.com 2026-08-08: bonds/TQOB, PREVPRICE 54.254 for
		// 2026-08-07, SHORTNAME «ОФЗ 26238», ISIN RU000A1038V6. The foreign
		// instruments below are not on MOEX and keep their own exchanges'
		// tickers.
		{"SU26238RMFS4", instrument.Instrument{
			Type: instrument.TypeBond, Name: "ОФЗ 26238", Ticker: "SU26238RMFS4", Currency: "RUB",
			FaceValueMinor: &faceValue, FaceCurrency: &faceCurrency,
		}},
		{"FXUS", instrument.Instrument{Type: instrument.TypeETF, Name: "FinEx FXUS", Ticker: "FXUS", Currency: "USD", Frozen: true}},
		{"AAPL", instrument.Instrument{Type: instrument.TypeShare, Name: "Apple Inc.", Ticker: "AAPL", Currency: "USD"}},
		{"MSFT", instrument.Instrument{Type: instrument.TypeShare, Name: "Microsoft", Ticker: "MSFT", Currency: "USD"}},
		{"TSLA", instrument.Instrument{Type: instrument.TypeShare, Name: "Tesla", Ticker: "TSLA", Currency: "USD"}},
		{"NVDA", instrument.Instrument{Type: instrument.TypeShare, Name: "NVIDIA", Ticker: "NVDA", Currency: "USD"}},
		{"GOOGL", instrument.Instrument{Type: instrument.TypeShare, Name: "Alphabet", Ticker: "GOOGL", Currency: "USD"}},
		{"KAZ32EUR", instrument.Instrument{
			Type: instrument.TypeBond, Name: "Еврооблигация Казахстан 2032", Ticker: "KAZ32EUR", Currency: "USD",
			FaceValueMinor: &eurFaceValue, FaceCurrency: &eurFaceCurrency,
		}},
		{"WEWKQ", instrument.Instrument{Type: instrument.TypeShare, Name: "WeWork", Ticker: "WEWKQ", Currency: "USD"}},
		{"INTC", instrument.Instrument{Type: instrument.TypeShare, Name: "Intel", Ticker: "INTC", Currency: "USD"}},
		// Amazon carries the journal's two transfer demonstrations side by side:
		// two parcels bought at Т-Банк and moved to Freedom KZ one at a time, so
		// the destination shows two same-day arrivals saying different things.
		{"AMZN", instrument.Instrument{Type: instrument.TypeShare, Name: "Amazon", Ticker: "AMZN", Currency: "USD"}},
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

	// Chronological: operation.Service.Create replays each journal and
	// refuses, say, a sell before its buy.
	ops := []operation.Operation{
		{
			AccountID: tbank, Type: operation.TypeDeposit,
			OccurredOn: d("2026-05-05"), AmountMinor: 1_500_000_00, Currency: "RUB",
		},
		{
			AccountID: freedom, Type: operation.TypeDeposit,
			OccurredOn: d("2026-05-06"), AmountMinor: 4_000_000, Currency: "USD",
		},
		// Apple is bought twice, the first inside the gap before the fx history
		// (see seededUSDRates), so the whole position declines to show roubles
		// rather than a basis from one lot; it stays in dollars marked "not
		// converted" (displayCurrency.notConverted).
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
		// Amazon's first parcel, dated in the fx gap, puts two different sentences
		// three rows apart on the Т-Банк journal (#79):
		//
		// 	this buy          — money moved on 11.05.2026, which has no rate:
		// 	                    «Нет курса на дату операции» (no_rate_operation_date)
		// 	the transfer      — the basis valued at its purchase day, which has no
		// 	                    rate: no_rate_lot_date; the transfer's own
		// 	                    20.07.2026 rate (78.50) exists and may not value
		// 	                    shares bought in May
		//
		// 11.05.2026 is an ordinary Monday; only the demo's fx table is missing.
		{
			AccountID: tbank, InstrumentID: inst("AMZN"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-11"), Quantity: qty("10"), Price: price("180"),
			AmountMinor: -180_000, Currency: "USD",
		},
		// The ordinary bond, where the trade dialog's two price fields agree (#77).
		// Money per bond is recorded; the percentage is what the dialog asks first:
		//
		// 	номинал                            1 000,00 ₽
		// 	«Цена, % от номинала»                  95,00 %
		// 	«Цена за одну облигацию»              950,00 ₽ = 1 000,00 × 95 ÷ 100
		// 	итог  100 × 950,00 ₽             = 95 000,00 ₽ = 9_500_000 minor
		//
		// Both directions land exactly (bondPriceFromPercent, bondPercentFromPrice in
		// web/src/lib/money.ts), so re-entering the trade gives this row back. The fee,
		// 0,1 % = 95,00 ₽, is a field of its own.
		//
		// The quote is 95,20 %, 952,00 ₽ a bond, 95 200,00 ₽ for 100. The engine
		// capitalizes the fee into the lot (addLot), so the basis is 95 095,00 ₽ and
		// the row reads +105,00 ₽ (+0,1 %), not +200,00 ₽. The only seeded buy with a
		// fee.
		{
			AccountID: tbank, InstrumentID: inst("SU26238RMFS4"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-12"), Quantity: qty("100"), Price: price("950"),
			AmountMinor: -9_500_000, FeeMinor: 9_500, Currency: "RUB",
		},
		// TSLA: two lots at Т-Банк on two dates and rates (60.00, 64.00), later
		// transferred whole to Freedom KZ, so the receiving account must keep each
		// lot's purchase date. Both rates are below the transfer day's 78.50, so
		// collapsing onto the transfer day overvalues unmistakably. Arithmetic at
		// the transfer.
		{
			AccountID: tbank, InstrumentID: inst("TSLA"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-13"), Quantity: qty("5"), Price: price("180"),
			AmountMinor: -90_000, Currency: "USD",
		},
		// NVDA, the earliest of three parcels: bought at Т-Банк and transferred
		// into an account already holding a later one. A sale there takes this
		// one first, by purchase day rather than arrival. Arithmetic at the
		// transfer.
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
		// The sub-cent price (#30): bought at $0,40, quoted at $0,0025 after
		// bankruptcy. Two decimals would print «0,00»; the price line shows
		// «0,0025». A share, because marketValue values only shares, bonds and
		// ETFs.
		//
		// 	cost   5 000 × $0,40   = $2 000,00 =  200_000 minor USD, 2026-05-20
		// 	  in ₽ 200_000 × 79.15 (that day's own rate) = 15_830_000 = 158 300,00 ₽
		// 	value  5 000 × $0,0025 =    $12,50 =    1_250 minor USD
		// 	  in ₽   1_250 × 78.50 (today's rate)      =      98_125 =     981,25 ₽
		//
		// 	profit in USD =  1_250 −    200_000 =    −198_750  (−$1 987,50, −99,4 %)
		// 	profit in RUB = 98_125 − 15_830_000 = −15_731_875  (−157 318,75 ₽, −99,4 %)
		{
			AccountID: freedom, InstrumentID: inst("WEWKQ"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-20"), Quantity: qty("5000"), Price: price("0.40"),
			AmountMinor: -200_000, Currency: "USD",
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
		// The open position whose profit has opposite signs in dollars and roubles
		// (owner's decision 2026-07-29: rouble return includes the currency's move).
		// Bought on a weak-rouble day, valued at a stronger rouble; no fee.
		//
		// 	cost    20 × $500.00 = $10 000.00 =   1_000_000 minor USD
		// 	  in ₽  1_000_000 × 81.40 (2026-06-10, the lot's own day) = 81_400_000 = 814 000,00 ₽
		// 	value   20 × $510.00 = $10 200.00 =   1_020_000 minor USD
		// 	  in ₽  1_020_000 × 78.50 (today, 2026-07-20) = 80_070_000 = 800 700,00 ₽
		//
		// 	profit in USD =  1_020_000 −  1_000_000 =    +20_000  (+$200.00, +2.0 %)
		// 	profit in RUB = 80_070_000 − 81_400_000 = −1_330_000  (−13 300,00 ₽, −1.6 %)
		//
		// Real cbr.ru rates from the backfill would change these figures.
		{
			AccountID: freedom, InstrumentID: inst("MSFT"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-10"), Quantity: qty("20"), Price: price("500"),
			AmountMinor: -1_000_000, Currency: "USD",
		},
		// Alphabet: the closed deal whose settled result has opposite signs in
		// dollars and roubles; MSFT is its open twin. Expense at the purchase day's
		// rate, proceeds at the sale day's (НК РФ ст. 210 п. 5), no fees:
		//
		// 	buy   50 × $200.00 = $10 000.00 =  1_000_000 minor USD, 2026-06-10
		// 	  in ₽  1_000_000 × 81.40 (the purchase day) = 81_400_000 = 814 000,00 ₽
		// 	sell  50 × $210.00 = $10 500.00 =  1_050_000 minor USD, 2026-06-20
		// 	  in ₽  1_050_000 × 65.00 (the sale day)     = 68_250_000 = 682 500,00 ₽
		//
		// 	result in USD =  1_050_000 −  1_000_000 =     +50_000  (+$500.00)
		// 	result in RUB = 68_250_000 − 81_400_000 = −13_150_000  (−131 500,00 ₽)
		//
		// Any single rate would show a profit. The account's «Зафиксировано» line:
		//
		// 	NVDA, sold 2026-07-22   +100_000 USD    +9_650_000 ₽
		// 	  = 200_000 × 78.50 − 100_000 × 60.50 (the sale day resolving to
		// 	    2026-07-20's rate, the basis to its own 2026-05-14)
		// 	Alphabet, sold 2026-06-20  +50_000 USD  −13_150_000 ₽
		// 	account total             +150_000 USD   −3_500_000 ₽
		// 	                        (+$1 500.00)     (−35 000,00 ₽)
		//
		// so the account's total flips sign across the display toggle. The other six
		// Freedom KZ positions have no disposals and contribute nothing (which also
		// keeps AAPL's and Intel's gaps out of the total). The whole parcel is sold so
		// the balance above is unaffected. Real rates would change the figures.
		{
			AccountID: freedom, InstrumentID: inst("GOOGL"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-10"), Quantity: qty("50"), Price: price("200"),
			AmountMinor: -1_000_000, Currency: "USD",
		},
		// Amazon's second parcel, where the valuation day and the rate's day differ
		// (#80). 12 June is Russia Day (a Friday in 2026): no CBR rate, while New York
		// trades. The table carries 2026-06-11:
		//
		// 	cost  10 × $200,00 = $2 000,00 =    200_000 minor USD, dated 12.06.2026
		// 	  in ₽ 200_000 × 81.00 (the rate of 11.06.2026) = 16_200_000 = 162 000,00 ₽
		//
		// dated_on 2026-06-12, rate_on 2026-06-11; the tooltip reads «На дату операции
		// курса нет — пересчитано по ближайшему, на 11.06.2026». On the transfer below
		// the sentence is about a purchase, so it must name 12.06. No fee; 81.00 is
		// distinct from its neighbours so a wrong lookup shows.
		{
			AccountID: tbank, InstrumentID: inst("AMZN"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-12"), Quantity: qty("10"), Price: price("200"),
			AmountMinor: -200_000, Currency: "USD",
		},
		// TSLA's second lot; see the first.
		{
			AccountID: tbank, InstrumentID: inst("TSLA"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-15"), Quantity: qty("5"), Price: price("200"),
			AmountMinor: -100_000, Currency: "USD",
		},
		// Intel is bought only so it can leave: the transfer below moves it with a
		// hand-typed basis (a transfer needs a source holding the instrument), and
		// the dateless lot it becomes at Freedom KZ is the demonstration.
		{
			AccountID: tbank, InstrumentID: inst("INTC"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-15"), Quantity: qty("100"), Price: price("30"),
			AmountMinor: -300_000, Currency: "USD",
		},
		{
			AccountID: tbank, InstrumentID: inst("SU26238RMFS4"), Type: operation.TypeCoupon,
			OccurredOn: d("2026-06-18"), AmountMinor: 354_000, Currency: "RUB",
		},
		// NVDA's second parcel, bought at Freedom KZ after the Т-Банк one and
		// before it arrives: first by arrival, second by purchase. The sale below
		// takes the transferred one.
		{
			AccountID: freedom, InstrumentID: inst("NVDA"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-20"), Quantity: qty("10"), Price: price("150"),
			AmountMinor: -150_000, Currency: "USD",
		},
		// The sale closing Alphabet; see its buy.
		{
			AccountID: freedom, InstrumentID: inst("GOOGL"), Type: operation.TypeSell,
			OccurredOn: d("2026-06-20"), Quantity: qty("50"), Price: price("210"),
			AmountMinor: 1_050_000, Currency: "USD",
		},
		// The third-currency holding (#39): a €1 000,00-face eurobond traded in
		// dollars in a rouble space. A bond's quote is a percentage of face, so its
		// value is in euros (portfolio.marketValue) and is converted twice, both from
		// the euro figure, never chained through dollars:
		//
		// 	valuation  €1 000,00 × 98,00 % × 5 = €4 900,00 =    490_000 minor EUR
		// 	  in $     490_000 × (92.30 ÷ 78.50) =    576_140 = $5 761,40
		// 	  in ₽     490_000 × 92.30           = 45_227_000 = 452 270,00 ₽
		// 	cost       5 × $1 150,00 = $5 750,00 =    575_000 minor USD, 2026-06-20
		// 	  in ₽     575_000 × 65.00 (that day's own rate) = 37_375_000 = 373 750,00 ₽
		//
		// 	profit in USD =    576_140 −    575_000 =    +1_140  (+$11,40, +0,2 %)
		// 	profit in RUB = 45_227_000 − 37_375_000 = +7_852_000  (+78 520,00 ₽, +21,0 %)
		//
		// Chaining through dollars would give 45_226_990, ten kopecks short. The price
		// tooltip reads «Пересчитано из 4 900,00 €», and the price line «98,00 %» (#32).
		//
		// The buy's price is money per unit, $1 150,00, near what such a bond trades at.
		// Re-entered in the trade dialog the percent field is disabled with «Номинал в
		// EUR, а сделка в USD: цену в валюте сделки из процента не получить — курса здесь
		// нет. Введите цену за одну бумагу» (#77); the OFZ is the same dialog with the
		// link intact.
		{
			AccountID: freedom, InstrumentID: inst("KAZ32EUR"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-20"), Quantity: qty("5"), Price: price("1150"),
			AmountMinor: -575_000, Currency: "USD",
		},
		// A dollar deposit on a rouble account on a Saturday: no CBR rate, so it
		// converts at Friday's and says so. With the FXUS buy it also gives this
		// journal two USD entries on two dates, which is what brings up the
		// display-currency toggle.
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

	opSvc := operation.NewService(operation.NewStore(tx))
	for _, op := range ops {
		if _, err := opSvc.Create(ctx, spaceID, op); err != nil {
			return fmt.Errorf("seed operation %s %s: %w", op.Type, op.OccurredOn.Format("2006-01-02"), err)
		}
	}

	// TSLA moves whole from Т-Банк to Freedom KZ on 2026-07-20, the seed's
	// "today", through CreateTransfer, which releases the source lots and carries
	// their purchase dates (Service.Create refuses transfer legs):
	//
	// 	lot 1: 5 @ $180.00 on 2026-05-13 -> 90_000 minor USD
	// 	  at its own day's rate (60.00): 90_000 * 60.00 =  5_400_000 =  54 000,00 ₽
	// 	lot 2: 5 @ $200.00 on 2026-06-15 -> 100_000 minor USD
	// 	  at its own day's rate (64.00): 100_000 * 64.00 =  6_400_000 =  64 000,00 ₽
	//
	// 	destination cost_minor (USD)            =  90_000 + 100_000        =    190_000 (= $1 900.00)
	// 	in_base.cost_minor (per lot)            = 5_400_000 + 6_400_000    = 11_800_000 (= 118 000,00 ₽)
	//
	// 	at the transfer day instead (78.50): 190_000 * 78.50 = 14_915_000
	// 	  = 149 150,00 ₽, 31 150,00 ₽ (≈26 %) invented by the move
	//
	// Both journal rows read 118 000,00 ₽ as well (see
	// operation.Handler.operationInBase).
	if _, _, err := opSvc.CreateTransfer(ctx, spaceID, operation.TransferParams{
		FromAccountID: tbank, ToAccountID: freedom, InstrumentID: instIDs["TSLA"],
		Quantity: decimal.RequireFromString("10"), OccurredOn: d("2026-07-20"),
	}); err != nil {
		return fmt.Errorf("seed transfer TSLA: %w", err)
	}

	// NVDA moves to Freedom KZ, which already holds a later-bought parcel; two
	// days later half the holding is sold:
	//
	// 	arriving parcel: 10 @ $100.00 bought 2026-05-14 at Т-Банк  -> 100_000 minor USD
	// 	parcel already here: 10 @ $150.00 bought 2026-06-20        -> 150_000
	// 	sale: 10 @ $200.00 on 2026-07-22                           -> 200_000
	//
	// 	by purchase day (НК РФ ст. 214.1 п. 13; 26 CFR 1.1012-1(c)(1)(i)):
	// 	  realized P&L  = 200_000 − 100_000 = +100_000 (+$1 000.00)
	// 	  cost_minor    = 150_000 ($1 500.00), one lot, dated 2026-06-20
	// 	  in_base       = 150_000 × 65.00 = 9_750_000 (97 500,00 ₽)
	//
	// 	by arrival (wrong):
	// 	  realized P&L  = 200_000 − 150_000 =  +50_000 (+$500.00)
	// 	  cost_minor    = 100_000 ($1 000.00), dated 2026-05-14
	// 	  in_base       = 100_000 × 60.50 = 6_050_000 (60 500,00 ₽)
	//
	// Round and far apart so the difference is visible by eye.
	if _, _, err := opSvc.CreateTransfer(ctx, spaceID, operation.TransferParams{
		FromAccountID: tbank, ToAccountID: freedom, InstrumentID: instIDs["NVDA"],
		Quantity: decimal.RequireFromString("10"), OccurredOn: d("2026-07-20"),
	}); err != nil {
		return fmt.Errorf("seed transfer NVDA: %w", err)
	}

	// The dateless parcel. Two positions on one screen have no rouble figures for
	// different reasons (#66):
	//
	// 	Apple  — no rate for one lot's purchase date; the backfill will close it.
	// 	Intel  — no purchase date at all; it never closes.
	//
	// The basis is given by hand (TransferParams.CostMinorOverride), as for a parcel
	// from a broker that reported only a total: nothing is released, so the arriving
	// lot has no date (see portfolio.Lot.AcquiredOn).
	//
	// 	hand-entered basis       $3 000,00 = 300_000 minor USD, and no date at all
	// 	valuation 100 × $34,00 = $3 400,00 = 340_000 minor USD
	// 	profit in USD = 340_000 − 300_000 =  +40_000 (+$400,00, +13,3 %)
	// 	in rubles     = nothing on all four figures (Handler.positionInBase)
	//
	// Routed through Т-Банк because a transfer needs a source holding the paper.
	handEnteredBasis := int64(300_000)
	if _, _, err := opSvc.CreateTransfer(ctx, spaceID, operation.TransferParams{
		FromAccountID: tbank, ToAccountID: freedom, InstrumentID: instIDs["INTC"],
		Quantity: decimal.RequireFromString("100"), OccurredOn: d("2026-07-20"),
		CostMinorOverride: &handEnteredBasis,
	}); err != nil {
		return fmt.Errorf("seed transfer INTC: %w", err)
	}

	// The two Amazon transfers: one paper, one day, two arrivals at Freedom KZ
	// saying different things, so the difference can only come from the purchases.
	// Two transfers, because a row's base-currency block is all or nothing and one
	// parcel holding the undatable 11.05 lot would hide the 12.06 lot's figure. Each
	// releases the earliest purchase still held, and the second sees the first
	// (journalUpTo).
	//
	// 	first  10 @ $180,00 bought 11.05.2026 -> 180_000 minor USD ($1 800,00)
	// 	  in ₽ nothing: no rate on or before 11.05.2026, so both legs name a
	// 	       missing rate for a purchase day, not the transfer's 20.07.2026
	// 	second 10 @ $200,00 bought 12.06.2026 -> 200_000 minor USD ($2 000,00)
	// 	  in ₽ 200_000 × 81.00 (the rate of 11.06.2026) = 16_200_000 = 162 000,00 ₽
	//
	// On the second the tooltip («Это стоимость покупок … Самый поздний из них — на
	// …») names 12.06.2026 while the rate is of 11.06.2026 (#80), and 162 000,00 ₽
	// matches the buy it carries; at the transfer day's rate it would be
	// 157 000,00 ₽. At Freedom KZ the two make one 20-share position with no rouble
	// figure, consistent with the rows.
	if _, _, err := opSvc.CreateTransfer(ctx, spaceID, operation.TransferParams{
		FromAccountID: tbank, ToAccountID: freedom, InstrumentID: instIDs["AMZN"],
		Quantity: decimal.RequireFromString("10"), OccurredOn: d("2026-07-20"),
	}); err != nil {
		return fmt.Errorf("seed transfer AMZN (2026-05-11 parcel): %w", err)
	}
	if _, _, err := opSvc.CreateTransfer(ctx, spaceID, operation.TransferParams{
		FromAccountID: tbank, ToAccountID: freedom, InstrumentID: instIDs["AMZN"],
		Quantity: decimal.RequireFromString("10"), OccurredOn: d("2026-07-20"),
	}); err != nil {
		return fmt.Errorf("seed transfer AMZN (2026-06-12 parcel): %w", err)
	}

	// After the transfer: the parcel this sale consumes arrives with it.
	if _, err := opSvc.Create(ctx, spaceID, operation.Operation{
		AccountID: freedom, InstrumentID: inst("NVDA"), Type: operation.TypeSell,
		OccurredOn: d("2026-07-22"), Quantity: qty("10"), Price: price("200"),
		AmountMinor: 200_000, Currency: "USD",
	}); err != nil {
		return fmt.Errorf("seed operation sell NVDA: %w", err)
	}

	// The long journal: 34 monthly top-ups and service charges on Т-Банк, so its
	// journal runs past one page (#86). The client pages by 50 (JOURNAL_PAGE_SIZE,
	// web/src/api/operations.ts); Т-Банк already has 21 rows, so 55 makes a full
	// first page with "show more" and a five-row second. Dated before every scenario
	// (2024-12..2026-04), so the first page opens on the scenarios. Roubles with no
	// instrument, so they convert nothing and touch no position; balances are
	// recorded, never summed from the journal. Anchored on the 1st so the 5th and
	// 28th exist every month.
	fillerFirstMonth := d("2024-12-01")
	for i := 0; i < 17; i++ {
		month := fillerFirstMonth.AddDate(0, i, 0)
		if _, err := opSvc.Create(ctx, spaceID, operation.Operation{
			AccountID: tbank, Type: operation.TypeDeposit,
			OccurredOn: month.AddDate(0, 0, 4), AmountMinor: 30_000_00, Currency: "RUB",
			Note: "Ежемесячное пополнение",
		}); err != nil {
			return fmt.Errorf("seed monthly top-up %s: %w", month.Format("2006-01"), err)
		}
		if _, err := opSvc.Create(ctx, spaceID, operation.Operation{
			AccountID: tbank, Type: operation.TypeFee,
			OccurredOn: month.AddDate(0, 0, 27), AmountMinor: -290_00, Currency: "RUB",
			Note: "Обслуживание брокерского счёта",
		}); err != nil {
			return fmt.Errorf("seed monthly service fee %s: %w", month.Format("2006-01"), err)
		}
	}

	if err := seedMarketData(ctx, tx, instIDs, d); err != nil {
		return err
	}
	return nil
}

// seededUSDRates is the demo's USD/RUB history; each date demonstrates
// something:
//
//   - 2026-05-13, 2026-06-15: the TSLA lots (60.00, 64.00), below the transfer
//     day's 78.50 so collapsing onto it overvalues clearly. 2026-06-15 is also
//     Intel's buy, which converts only on that Т-Банк row.
//   - 2026-05-14, 2026-06-20: the NVDA parcels (60.50, 65.00); the surviving
//     parcel's basis is 97 500,00 ₽ where arrival order would leave 60 500,00 ₽.
//     2026-06-20 is also the Alphabet sale (65.00 against 81.40) and the
//     eurobond's buy (373 750,00 ₽).
//   - 2026-05-20: FXUS and WeWork, converted at their exact date.
//   - 2026-06-10: AAPL, MSFT and Alphabet buys at the highest rate, 81.40, which
//     turns a dollar profit into a rouble loss for MSFT (on paper) and Alphabet
//     (for good).
//   - 2026-06-11: the eve of Russia Day, the rate a purchase on the 12th
//     reaches (81.00, distinct from 81.40), the second Amazon parcel (#80).
//   - 2026-07-03: the Friday before the Saturday deposit.
//   - 2026-07-20: "today", shared with the latest balances, most quotes,
//     GET /summary and the three transfers; with EUR/RUB it bridges the
//     eurobond's EUR->USD (92.30 ÷ 78.50, marketdata.resolveRate).
//
// Nothing before 2026-05-13, so three operations have no rate at all (lookups
// fall back only to earlier dates): the Freedom KZ deposit (2026-05-06) shows its
// original amount; the first AAPL lot (2026-05-08) keeps the whole position out of
// roubles; the first Amazon parcel (2026-05-11) makes its transfer name a missing
// purchase-day rate (#79). On a connected instance the backfill fills them.
var seededUSDRates = []struct{ on, rate string }{
	{"2026-05-13", "60.00"},
	{"2026-05-14", "60.50"},
	{"2026-05-20", "79.15"},
	{"2026-06-10", "81.40"},
	{"2026-06-11", "81.00"},
	{"2026-06-15", "64.00"},
	{"2026-06-20", "65.00"},
	{"2026-07-03", "77.90"},
	{"2026-07-20", "78.50"},
}

// seedMarketData records the fx rates and quotes the demo's valuations need:
// GET /summary's total, positions' market values, the journal's per-row in_base.
//
// EUR and KZT are pinned to 2026-07-20, the demo's "now". USD/RUB is a history
// with a different rate per date (see seededUSDRates), or historical and
// current conversions would look identical. EUR/RUB also carries the eurobond's
// valuation, in roubles and, bridged, in dollars.
//
// A quote's date is its trading session (marketdata.TickerQuote.On), never the
// fetch day (#90): 2026-07-20 (a Monday) for all but WeWork. It only picks the
// latest row and feeds the «Цена на …» caption.
//
// FXUS and AAPL get no quote, so their positions show the no-valuation path. FXUS
// is marked Frozen, but nothing in valuation reads that flag (#85): it is a badge.
// AAPL has no provider: cbr and moex cover the Russian market only. MSFT, KAZ32EUR,
// WEWKQ, INTC and AMZN are hand-quoted because each demonstration needs a
// valuation; see each quote.
func seedMarketData(ctx context.Context, tx pgx.Tx, instIDs map[string]uuid.UUID, d func(string) time.Time) error {
	mdStore := marketdata.NewStore(tx)
	// The demo's "now": the newest fx rate and the newest balance day.
	on := d("2026-07-20")
	// The session most quotes belong to; the same day as "now" by choice, a
	// Monday so it reads as a session.
	session := d("2026-07-20")
	// WeWork's session, ten days earlier; see its quote.
	weworkSession := d("2026-07-10")
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

	// The OFZ quote is a percentage of face (95.20 = 95,20 %): 952,00 ₽ a bond
	// against 950,00 ₽ paid (see the OFZ buy). SBER, LKOH and the OFZ are real
	// MOEX tickers, so on a connected instance the exchange's prices replace
	// these.
	quotes := []marketdata.Quote{
		{InstrumentID: instIDs["SBER"], On: session, Price: rate("305.50"), Currency: "RUB", Source: "seed"},
		{InstrumentID: instIDs["LKOH"], On: session, Price: rate("7550.00"), Currency: "RUB", Source: "seed"},
		{InstrumentID: instIDs["SU26238RMFS4"], On: session, Price: rate("95.20"), Currency: "RUB", Source: "seed"},
		{InstrumentID: instIDs["MSFT"], On: session, Price: rate("510.00"), Currency: "USD", Source: "seed"},
		// NVDA at the sale price: $2 000.00 against a $1 500.00 basis, +$500.00
		// unrealized beside the +$1 000.00 realized in «Зафиксировано».
		{InstrumentID: instIDs["NVDA"], On: session, Price: rate("200.00"), Currency: "USD", Source: "seed"},
		// The eurobond quoted as a percentage of face. Currency is the unit of the
		// percentage; the money is in the face currency, which marketValue reads.
		// EUR so the two agree, as the OFZ's RUB does.
		{InstrumentID: instIDs["KAZ32EUR"], On: session, Price: rate("98.00"), Currency: "EUR", Source: "seed"},
		// The only sub-cent price (#30; see the WEWKQ buy), and the only quote on a
		// different session: the screen shows «Цена на 10.07.2026» beside «Цена на
		// 20.07.2026», so the caption is visibly about the price. WeWork rather than a
		// Russian paper, because MOEX dates a whole board's session at once (TQBR's 502
		// rows share one PREVDATE; see QuotesFor), and an unpriced foreign paper keeps
		// its old row anyway. Nothing computes from a quote's date.
		{InstrumentID: instIDs["WEWKQ"], On: weworkSession, Price: rate("0.0025"), Currency: "USD", Source: "seed"},
		// Intel is quoted so its dateless row withholds all four rouble figures,
		// not two.
		{InstrumentID: instIDs["INTC"], On: session, Price: rate("34.00"), Currency: "USD", Source: "seed"},
		// Amazon at $209,00: 20 shares cost $3 800,00 and are worth $4 180,00,
		// +$380,00 (+10,0 %). In roubles nothing is published: one lot was bought on
		// 2026-05-11, outside the fx table, and the block goes whole or not at
		// all.
		{InstrumentID: instIDs["AMZN"], On: session, Price: rate("209.00"), Currency: "USD", Source: "seed"},
	}
	if err := mdStore.UpsertQuotes(ctx, quotes); err != nil {
		return fmt.Errorf("seed quotes: %w", err)
	}
	return nil
}
