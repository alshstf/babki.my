package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
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
			ctx, stop := signalCtx()
			defer stop()
			// requireEncryptionKey=false: seed writes plain demo data and
			// never touches an encrypted secret, so it must keep working on
			// a machine where BABKI_ENCRYPTION_KEY hasn't been provisioned.
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
			// doc), so it has to exceed what the seeded positions cost:
			//
			//	AAPL                       $6 209,20
			//	MSFT                      $10 000,00
			//	TSLA (transferred in)      $1 900,00
			//	NVDA (what is left)        $1 500,00
			//	KAZ32EUR (the eurobond)    $5 750,00
			//	WEWKQ                      $2 000,00
			//	INTC (hand-entered basis)  $3 000,00
			//	AMZN (two transfers)       $3 800,00
			//	                          $34 159,20
			//
			// A balance below that would put a single position above the whole
			// account it lives in, right on the screen this data exists to show.
			// Alphabet is absent from that sum on purpose: its parcel was bought
			// and sold in full inside the period, so it holds nothing today and
			// weighs nothing against this balance — only its settled result
			// survives, in the account's «Зафиксировано» line.
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

	if err := seedInstrumentsAndOperations(ctx, pool, owner.SpaceID, accIDs, d); err != nil {
		return err
	}
	if err := seedTinvestDemo(ctx, pool, owner.SpaceID, accIDs["Брокерский Т-Банк"], d); err != nil {
		return err
	}
	return nil
}

// seedTinvestDemo gives the demo instance a T-Invest connection with something
// to show on every one of the importer's screens: the connection itself, the
// broker account it feeds, a run log, a difference the reconciliation found and
// operations the projection could not read.
//
// IT NEVER TOUCHES THE NETWORK, AND TWO THINGS TOGETHER GUARANTEE THAT. The
// connection is created `disabled`, which the hourly dispatcher skips (it lists
// active connections only) and which the «sync now» button refuses with a 409
// before anything is decrypted; and its stored token is random bytes rather
// than a sealed secret, so even a path that reached the decryption would stop
// there rather than talk to a broker with something that happened to work.
// Neither is load-bearing alone — together they mean the stand cannot make a
// broker call however it is poked.
//
// EVERY FIGURE IN THE RUN LOG IS THE MIRROR'S OWN. The counters below are what
// the two SyncMirror calls actually returned and what the unparsed rows
// actually number, not numbers chosen to look busy: a demo whose caption its
// own data contradicts is the exact failure this project keeps being bitten by,
// and it would be on the screen built to make the importer trustworthy.
//
// Its dates are fixed like the rest of the seed, with one exception that cannot
// be: a run's own started_at/finished_at are the database's clock at the moment
// the row is written (see Store.StartRun and FinishRun, which take no time).
// That is honest here rather than merely unavoidable — the run log answers "when
// did this import run", and seeding IS when this demo import ran; the operations
// it mirrors carry the fixed dates.
func seedTinvestDemo(ctx context.Context, pool *pgxpool.Pool, spaceID, accountID uuid.UUID,
	d func(string) time.Time,
) error {
	store := tinvest.NewStore(pool)

	// Random bytes, and deliberately NOT secretbox.Seal of anything: this seed
	// runs without BABKI_ENCRYPTION_KEY (see newSeedCmd), so there is no box to
	// seal with — and a demo instance must not hold a decryptable broker token
	// in any case. Sixty-four bytes so the column holds something of a
	// plausible size; nothing anywhere reads it.
	ciphertext := make([]byte, 64)
	if _, err := rand.Read(ciphertext); err != nil {
		return fmt.Errorf("seed tinvest connection: %w", err)
	}
	conn, err := store.CreateConnection(ctx, spaceID, ciphertext, "0000")
	if err != nil {
		return fmt.Errorf("seed tinvest connection: %w", err)
	}
	if err := store.UpdateConnectionStatus(ctx, conn.ID, tinvest.StatusDisabled); err != nil {
		return fmt.Errorf("seed tinvest connection status: %w", err)
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

	// Three operations the projection has no rules for, which is what the
	// unparsed screen exists to show: variation margin and an overnight repo
	// (owner's decision 5 — futures, options, margin and repo stay visible as
	// unparsed until there is live data to build rules from) and a purchase of
	// currency, which is a refusal of its own rather than an unsupported type.
	first := seedTinvestOperations(d)
	firstRun, err := store.StartRun(ctx, conn.ID, link.ID, tinvest.TriggerInitial)
	if err != nil {
		return fmt.Errorf("seed tinvest first run: %w", err)
	}
	firstStats, err := store.SyncMirror(ctx, conn.ID, link, first, d("2026-07-20").Add(9*time.Hour))
	if err != nil {
		return fmt.Errorf("seed tinvest mirror: %w", err)
	}
	rows, err := store.MirrorRowsByLink(ctx, link.ID)
	if err != nil {
		return fmt.Errorf("seed tinvest mirror rows: %w", err)
	}
	reasons := map[uuid.UUID]string{}
	for _, row := range rows {
		reasons[row.ID] = seedTinvestReasons[row.BrokerOperationID]
	}
	if err := store.SetUnparsedReasons(ctx, reasons); err != nil {
		return fmt.Errorf("seed tinvest unparsed reasons: %w", err)
	}
	if err := store.FinishRun(ctx, firstRun.ID, tinvest.RunOutcome{
		Status:           tinvest.RunOK,
		ReadCount:        firstStats.Read,
		AddedCount:       firstStats.Added,
		DisappearedCount: firstStats.Disappeared,
		UnparsedCount:    len(reasons),
	}); err != nil {
		return fmt.Errorf("seed tinvest first run outcome: %w", err)
	}

	// A second run in which the broker no longer returns the overnight repo.
	// The row is not deleted — it is marked, and it stays on the unparsed list,
	// which is the behaviour the mirror was built for and the one hardest to
	// believe without seeing it.
	second := first[:len(first)-1]
	secondRun, err := store.StartRun(ctx, conn.ID, link.ID, tinvest.TriggerSchedule)
	if err != nil {
		return fmt.Errorf("seed tinvest second run: %w", err)
	}
	secondStats, err := store.SyncMirror(ctx, conn.ID, link, second, d("2026-07-20").Add(10*time.Hour))
	if err != nil {
		return fmt.Errorf("seed tinvest mirror refresh: %w", err)
	}
	return store.FinishRun(ctx, secondRun.ID, tinvest.RunOutcome{
		Status:           tinvest.RunOK,
		ReadCount:        secondStats.Read,
		AddedCount:       secondStats.Added,
		DisappearedCount: secondStats.Disappeared,
		UnparsedCount:    len(reasons),
		// One difference, and a real one: the demo's Т-Банк account holds no
		// futures position of its own in the journal, because the variation
		// margin above never became a journal entry — which is exactly the
		// story the unparsed list beside it tells.
		Reconcile: tinvest.ReconcileResult{
			Status: tinvest.ReconcileMismatched,
			Mismatches: []tinvest.ReconcileMismatch{{
				Kind:    tinvest.MismatchUnsupported,
				Label:   "SiH6 (futures)",
				Broker:  decimal.NewFromInt(2),
				Journal: decimal.Zero,
			}},
		},
	})
}

// seedTinvestReasons says why each seeded broker operation did not become a
// journal entry, keyed by the broker's own operation id. It is a table beside
// the operations rather than a field inside them because that is how the two
// are actually written: the mirror takes what the broker said, and the
// projection's verdict is recorded onto those rows afterwards (see
// Store.SetUnparsedReasons).
var seedTinvestReasons = map[string]string{
	"seed-varmargin-1": string(tinvest.ReasonUnsupportedType),
	"seed-fxbuy-1":     string(tinvest.ReasonCurrencyTrade),
	"seed-overnight-1": string(tinvest.ReasonUnsupportedType),
}

// seedTinvestOperations is the broker's side of the demo import: what a
// GetOperationsByCursor page would have carried. Raw is written out in full
// because it is the whole point of the unparsed screen — a person asking what
// the broker actually sent gets the broker's own document, not this program's
// reading of it.
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

	// The OFZ's face value: a thousand rubles, on a bond whose operations are
	// in rubles too. Face currency and trade currency agreeing is exactly the
	// condition the trade dialog needs to turn a percentage of face into money
	// per bond, so this is the demo's bond where that link works — see its buy
	// below for the arithmetic both directions produce.
	faceValue := int64(1_000_00)
	faceCurrency := "RUB"
	// The eurobond's face value is denominated in a THIRD currency: not the
	// dollars its position trades in and not the rubles the space totals in.
	// That is the whole point of it — see the buy below (#39) — and it is also
	// the demo's example of the one thing the trade dialog refuses to do (#77):
	// a percentage of a EUR face value is euros, the trade is booked in
	// dollars, and that dialog holds no fx rate. So the percent field is
	// disabled there and a sentence names the mismatch, instead of a number
	// arrived at by multiplying one currency by another.
	eurFaceValue := int64(1_000_00)
	eurFaceCurrency := "EUR"
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
		{"GOOGL", instrument.Instrument{Type: instrument.TypeShare, Name: "Alphabet", Ticker: "GOOGL", Currency: "USD"}},
		{"KAZ32EUR", instrument.Instrument{
			Type: instrument.TypeBond, Name: "Еврооблигация Казахстан 2032", Ticker: "KAZ32EUR", Currency: "USD",
			FaceValueMinor: &eurFaceValue, FaceCurrency: &eurFaceCurrency,
		}},
		{"WEWKQ", instrument.Instrument{Type: instrument.TypeShare, Name: "WeWork", Ticker: "WEWKQ", Currency: "USD"}},
		{"INTC", instrument.Instrument{Type: instrument.TypeShare, Name: "Intel", Ticker: "INTC", Currency: "USD"}},
		// Amazon carries the journal's remaining two demonstrations, and it is
		// one instrument rather than two because both are about the SAME kind of
		// row — a transfer, whose amount is a basis assembled from purchases —
		// and are best read side by side. Its two parcels are bought at Т-Банк
		// and moved to Freedom KZ one at a time, so the destination journal ends
		// up with two arrivals of one ticker, on one day, saying different
		// things. See the two buys and the two transfers below.
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
			OccurredOn: d("2026-05-06"), AmountMinor: 4_000_000, Currency: "USD",
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
		// Amazon's FIRST parcel, and the older of the two this instrument moves
		// to Freedom KZ. It is dated inside the gap before the fx history starts
		// (see seededUSDRates), so no rate can be resolved for it at all — and
		// that one fact puts TWO DIFFERENT SENTENCES on the Т-Банк journal, three
		// rows apart, which is the whole reason this parcel exists:
		//
		//	this row, a buy    — the amount is money that moved on 11.05.2026 and
		//	                     THAT day has no rate: «Нет курса на дату
		//	                     операции» (in_base_gap = no_rate_operation_date).
		//	the transfer below — the amount is this parcel's cost basis, valued at
		//	                     the day it was BOUGHT, and that is the day with no
		//	                     rate: «стоимость покупок … нет курса на день одной
		//	                     из этих покупок» (no_rate_lot_date). The
		//	                     transfer's own date, 20.07.2026, has an exact rate
		//	                     of its own (78.50) — it is simply not a rate that
		//	                     may value shares bought in May, which is the whole
		//	                     of #79.
		//
		// Before this seed the second sentence was reachable only from a test:
		// every transfer here moved lots whose own days all had rates. The two
		// are the pair most easily mistaken for each other, so they are put on one
		// screen where the difference can be read rather than argued.
		//
		// 11.05.2026 is a Monday — an ordinary trading day, not a date chosen to
		// be strange. What is missing is the demo's fx table, not the market.
		{
			AccountID: tbank, InstrumentID: inst("AMZN"), Type: operation.TypeBuy,
			OccurredOn: d("2026-05-11"), Quantity: qty("10"), Price: price("180"),
			AmountMinor: -180_000, Currency: "USD",
		},
		// The demo's ordinary bond, and the one that shows the trade dialog's
		// two linked price fields agreeing (#77). What is RECORDED is money per
		// bond — 950,00 ₽ — the same unit every other buy in this journal
		// carries and the only price that ever goes on the wire. The percentage
		// is what a broker quotes and what the dialog now asks for first:
		//
		//	номинал                            1 000,00 ₽ (faceValue above)
		//	«Цена, % от номинала»                  95,00 %
		//	«Цена за одну облигацию»              950,00 ₽ = 1 000,00 × 95 ÷ 100
		//	итог  100 × 950,00 ₽             = 95 000,00 ₽ = 9_500_000 minor, the
		//	                                                 AmountMinor below
		//
		// Both directions of the link land exactly here — 95 typed into the
		// percent field derives "950.00", 950 typed into the money field derives
		// "95.00" back (bondPriceFromPercent and bondPercentFromPrice in
		// web/src/lib/money.ts) — and nothing rounds on the way: 95 % of a whole
		// number of kopecks is a whole number of kopecks. So a reader who
		// re-enters this trade in the dialog gets this row back, figure for
		// figure, instead of having to trust that he would.
		//
		// The fee is the broker's 0,1 %, 95,00 ₽. It is deliberately outside the
		// arithmetic above: the dialog's «Итого» is quantity × price and the fee
		// is a field of its own, exactly as it is here.
		//
		// The quote below is 95,20 %, so one bond is worth 952,00 ₽ — 95 200,00 ₽
		// (9_520_000 minor) for the row's 100 bonds. That is NOT 200,00 ₽ above
		// what the row's own cost basis paid, because the basis is not the
		// 95 000,00 ₽ price alone: the engine capitalises a buy's fee into the
		// lot (addLot in internal/portfolio/engine.go, -AmountMinor+FeeMinor),
		// and this is the only seeded buy that carries one. Basis is therefore
		// 9_509_500 minor — 95 095,00 ₽, the 95 000,00 ₽ paid plus the 95,00 ₽
		// fee above — so the row reads +105,00 ₽ (+0,1 %), not +200,00 ₽
		// (+0,2 %). Read from a seeded database, not from arithmetic on paper.
		// NVDA and KAZ32EUR below carry no fee, so their own profit comments
		// are the plain price difference and are unaffected by this.
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
		// The demo's SUB-CENT price (#30). Bought at $0,40 while there was still
		// a company behind the ticker and quoted at $0,0025 after it went
		// through bankruptcy: rendered with the usual two fraction digits that
		// quote prints as «0,00» — a figure that is neither the price nor zero,
		// one column away from the place this program refuses to publish numbers
		// it cannot vouch for. The price line under the valuation reads
		// «0,0025» instead, by significant digits.
		//
		// It is deliberately a SHARE. portfolio.marketValue has no valuation
		// model for crypto, currencies, metals or custom instruments, so rows of
		// those types carry no price line at all and a sub-cent quote on one of
		// them would be invisible on this screen — which is where the owner
		// reviews this.
		//
		//	cost   5 000 × $0,40   = $2 000,00 =  200_000 minor USD, 2026-05-20
		//	  in ₽ 200_000 × 79.15 (that day's own rate) = 15_830_000 = 158 300,00 ₽
		//	value  5 000 × $0,0025 =    $12,50 =    1_250 minor USD
		//	  in ₽   1_250 × 78.50 (today's rate)      =      98_125 =     981,25 ₽
		//
		//	profit in USD =  1_250 −    200_000 =    −198_750  (−$1 987,50, −99,4 %)
		//	profit in RUB = 98_125 − 15_830_000 = −15_731_875  (−157 318,75 ₽, −99,4 %)
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
		// Alphabet is the demo's CLOSED deal whose settled result has a
		// DIFFERENT SIGN in dollars and in rubles — plan 7b's whole point made
		// visible on demo data instead of only in a unit test. MSFT above is its
		// paper twin: same disagreement, but on a holding still open, so that
		// figure still moves with every quote and every day's rate. This one is
		// over. Both ends are past events with dates of their own, so the ruble
		// result below is final and will never change again.
		//
		// Bought on the day the ruble was WEAKEST in the seeded history (81.40,
		// see seededUSDRates) and sold ten days later when it had strengthened to
		// 65.00. The expense is taken at the rate of the day the shares were
		// bought and the proceeds at the rate of the day they were sold (НК РФ
		// ст. 210 п. 5), so the currency's own move between those two days is
		// part of the result — and here it is far larger than the deal itself.
		// No broker fee on either leg, so every figure is checkable by eye:
		//
		//	buy   50 × $200.00 = $10 000.00 =  1_000_000 minor USD, 2026-06-10
		//	  in ₽  1_000_000 × 81.40 (the PURCHASE day) = 81_400_000 = 814 000,00 ₽
		//	sell  50 × $210.00 = $10 500.00 =  1_050_000 minor USD, 2026-06-20
		//	  in ₽  1_050_000 × 65.00 (the SALE day)     = 68_250_000 = 682 500,00 ₽
		//
		//	result in USD =  1_050_000 −  1_000_000 =     +50_000  (+$500.00)
		//	result in RUB = 68_250_000 − 81_400_000 = −13_150_000  (−131 500,00 ₽)
		//
		// Converting the dollar result at any single rate — today's, the sale
		// day's, the purchase day's — would land somewhere between +32 500,00 ₽
		// and +40 700,00 ₽: a profit, in a deal that lost 131 500,00 ₽. That is
		// the difference this position exists to put on screen.
		//
		// It also decides the ACCOUNT's «Зафиксировано» line, which is where a
		// reader actually meets it (the per-row realized column was removed as
		// visual noise in the owner's 4c feedback and stays removed):
		//
		//	NVDA, sold 2026-07-22   +100_000 USD    +9_650_000 ₽
		//	  = 200_000 × 78.50 − 100_000 × 60.50, the sale day resolving to
		//	    2026-07-20's rate (nothing is published on 07-22, the ordinary
		//	    nearest-earlier rule) and the basis to its own 2026-05-14
		//	Alphabet, sold 2026-06-20  +50_000 USD  −13_150_000 ₽
		//	account total             +150_000 USD   −3_500_000 ₽
		//	                        (+$1 500.00)     (−35 000,00 ₽)
		//
		// So the account's own total disagrees in sign with itself across the
		// display-currency toggle, on real demo data, one click apart. The other
		// six positions at Freedom KZ — AAPL, MSFT, TSLA, the eurobond, WeWork
		// and the transferred Intel parcel — contribute exactly zero: none of
		// them has ever disposed of anything, and a transfer is not a disposal.
		// That is also what keeps this line computable at all: a position with
		// no disposals asks the rate table for nothing, so neither AAPL's
		// rate-less lot nor Intel's dateless one can stop the account's total.
		//
		// The whole parcel is sold, so this leaves NO holding — deliberately.
		// A position that survived the sale would add its basis to an account
		// whose recorded balance is already close to what it holds (see the
		// Freedom KZ balances), and the sign flip needs no open shares to show.
		//
		// Like every other figure here, this depends on the seeded rates staying
		// put: an instance that reaches cbr.ru backfills 2026-06-10 and
		// 2026-06-20 with reality and the arithmetic above stops being the
		// arithmetic on screen.
		{
			AccountID: freedom, InstrumentID: inst("GOOGL"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-10"), Quantity: qty("50"), Price: price("200"),
			AmountMinor: -1_000_000, Currency: "USD",
		},
		// Amazon's SECOND parcel, and the demo's row where THE DAY A FIGURE IS
		// VALUED AT AND THE DAY ITS RATE CAME FROM ARE DIFFERENT DAYS (#80).
		//
		// 12 июня is Russia Day — a non-working holiday, which in 2026 falls on a
		// Friday. The CBR sets no rate that day; the New York exchange trades as
		// usual, which is how a dollar share comes to be bought on a day the
		// ruble has no rate of its own. The seeded table therefore carries
		// 2026-06-11, the last working day before it (see seededUSDRates), and
		// the conversion resolves to that:
		//
		//	cost  10 × $200,00 = $2 000,00 =    200_000 minor USD, dated 12.06.2026
		//	  in ₽ 200_000 × 81.00 (the rate OF 11.06.2026) = 16_200_000 = 162 000,00 ₽
		//
		// So this row publishes dated_on = 2026-06-12 and rate_on = 2026-06-11,
		// and its tooltip reads «На дату операции курса нет — пересчитано по
		// ближайшему, на 11.06.2026». Naming 11.06 as the day the shares were
		// bought — which the journal did until #80 — would name a holiday's
		// eve for a holiday's purchase.
		//
		// The same pair of dates returns on the transfer below, where it matters
		// more: there the sentence's subject is a PURCHASE («Самый поздний из них
		// — на …»), so it must name 12.06.2026, the day the shares were bought,
		// and not 11.06.2026, the day the rate is from. That row is the only
		// place in this demo where the two dates differ on a transfer, and
		// therefore the only place a reader can see which of them the sentence
		// picked.
		//
		// No fee, so the ruble figure above is the whole of the row's amount and
		// can be checked by eye; and 81.00 is a rate of its own rather than a
		// copy of a neighbour, so a conversion that quietly used 10.06's 81.40 or
		// 15.06's 64.00 instead would print a visibly different number.
		{
			AccountID: tbank, InstrumentID: inst("AMZN"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-12"), Quantity: qty("10"), Price: price("200"),
			AmountMinor: -200_000, Currency: "USD",
		},
		// TSLA's second lot — see the first lot above for why the rates and
		// the transfer are shaped the way they are.
		{
			AccountID: tbank, InstrumentID: inst("TSLA"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-15"), Quantity: qty("5"), Price: price("200"),
			AmountMinor: -100_000, Currency: "USD",
		},
		// Intel is bought here for one reason only: so that it can LEAVE. The
		// transfer below moves the whole parcel to Freedom KZ with a basis typed
		// in BY HAND, and a transfer needs a source account that holds the
		// instrument (see operation.Service.CreateTransfer, which reads the
		// currency off the source journal before it looks at the override).
		// Nothing about this buy is on show; what is on show is the DATELESS lot
		// it turns into over there — see the transfer call for the arithmetic
		// and for why this seed can produce that lot no other way.
		{
			AccountID: tbank, InstrumentID: inst("INTC"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-15"), Quantity: qty("100"), Price: price("30"),
			AmountMinor: -300_000, Currency: "USD",
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
		// The sale that closes the Alphabet parcel and settles its result for
		// good — see the buy on 2026-06-10 for the arithmetic and for why this
		// deal exists.
		{
			AccountID: freedom, InstrumentID: inst("GOOGL"), Type: operation.TypeSell,
			OccurredOn: d("2026-06-20"), Quantity: qty("50"), Price: price("210"),
			AmountMinor: 1_050_000, Currency: "USD",
		},
		// The demo's THIRD-CURRENCY holding: a eurobond with a €1 000,00 face
		// value, traded and settled in dollars, inside a space that totals in
		// rubles. Three currencies, one row — the case #39 was about, and one
		// nothing in this seed exercised before: the only other bond here has a
		// ruble face value on a ruble position, where all three collapse into
		// one and none of this can go wrong.
		//
		// A bond is quoted as a PERCENTAGE OF ITS FACE VALUE, so the money that
		// quote stands for is denominated in the FACE currency, never the
		// position's (see portfolio.marketValue). The euro figure therefore has
		// to reach two places: dollars, so the row can subtract cost from
		// valuation, and rubles, so the row can be read beside the others. Those
		// are two conversions OF THE SAME EURO FIGURE, not a chain — the ruble
		// answer is struck from the euros and never from the dollars:
		//
		//	valuation  €1 000,00 × 98,00 % × 5 = €4 900,00 =    490_000 minor EUR
		//	  in $     490_000 × (92.30 ÷ 78.50) =    576_140 = $5 761,40
		//	  in ₽     490_000 × 92.30           = 45_227_000 = 452 270,00 ₽
		//	cost       5 × $1 150,00 = $5 750,00 =    575_000 minor USD, 2026-06-20
		//	  in ₽     575_000 × 65.00 (that day's own rate) = 37_375_000 = 373 750,00 ₽
		//
		//	profit in USD =    576_140 −    575_000 =    +1_140  (+$11,40, +0,2 %)
		//	profit in RUB = 45_227_000 − 37_375_000 = +7_852_000  (+78 520,00 ₽, +21,0 %)
		//
		// Chaining the two conversions — the pre-plan-9 path — carried the
		// DOLLAR figure onward instead: 576_140 × 78.50 = 45_226_990, ten
		// kopecks short of the euro answer, because rounding the euros to a
		// whole cent of a currency that appears in no answer throws away a
		// fraction the second rate then multiplies. Ten kopecks is the entire
		// visible difference and it is meant to be: a EUR->USD rate is itself
		// bridged through the ruble here, so both paths ultimately stand on the
		// same two rows of the fx table and can only differ by that rounding.
		// What the row is really here to show is the price line's own TOOLTIP —
		// «Пересчитано из 4 900,00 €», appended to the hint that hovering the
		// price shows, not a line printed under it — which now describes a
		// conversion the ruble figure has actually been through.
		//
		// The price line itself reads «98,00 %», not «98,00»: the quote is a
		// percentage of face, and one of these bonds is worth €980,00 rather
		// than the €98,00 a bare number sitting under a money figure reads as
		// (#32).
		//
		// The buy's own price is money per unit, $1 150,00, the way every other
		// buy in this journal is recorded (the OFZ above does the same) — the
		// percentage convention belongs to the QUOTE, not to what the owner
		// paid. At today's rates €980,00 is about $1 152, so the deal is priced
		// where such a bond would actually have traded.
		//
		// Re-entering this trade in the dialog is where the second half of #77
		// becomes visible. «Цена, % от номинала» is greyed out and the sentence
		// under the pair reads «Номинал в EUR, а сделка в USD: цену в валюте
		// сделки из процента не получить — курса здесь нет. Введите цену за одну
		// бумагу» — the mismatch named, rather than a number produced by
		// multiplying a euro face value into a dollar trade. $1 150,00 then goes
		// in by hand under «Цена за одну облигацию (USD)», which is precisely
		// what this row records. The OFZ above is the same dialog with the link
		// intact: two bonds, one screen, and the difference between them stated
		// rather than silently absorbed.
		{
			AccountID: freedom, InstrumentID: inst("KAZ32EUR"), Type: operation.TypeBuy,
			OccurredOn: d("2026-06-20"), Quantity: qty("5"), Price: price("1150"),
			AmountMinor: -575_000, Currency: "USD",
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

	// The demo's DATELESS PARCEL — and the second half of the pair this branch
	// exists for. Two positions on ONE screen have no ruble figures, for two
	// DIFFERENT reasons, so the two sentences can be read side by side:
	//
	//	Apple  — no fx rate for one lot's purchase date. Its 2026-05-08 buy
	//	         predates everything in seededUSDRates, and the backfill job
	//	         will fill that date from cbr.ru: the gap CLOSES ON ITS OWN.
	//	Intel  — no purchase date for the parcel at all. Nobody recorded it and
	//	         nothing can recover it: the gap NEVER CLOSES.
	//
	// Both rows used to say «Нет курса — показано в исходной валюте». On the
	// second one that named a cause that is not the cause and promised a number
	// that is never coming (#66). They now say different things, and the demo
	// is where that difference is visible without opening a test file.
	//
	// The basis is given BY HAND — TransferParams.CostMinorOverride, the
	// `cost_minor` field of POST /operations/transfer — which is what a parcel
	// arriving from a broker that reported a total and nothing else looks like.
	// Nothing is released from the source in that case, so there are no source
	// lots behind the number and no acquisition dates to carry, and the
	// arriving lot is created with NONE. Not the transfer's own date: a lot's
	// date claims to say when its shares were bought, and 2026-07-20 is the day
	// the paperwork moved (see portfolio.Lot.AcquiredOn and Compute's
	// TypeTransferIn branch, where that lot is actually built).
	//
	//	hand-entered basis       $3 000,00 = 300_000 minor USD, and no date at all
	//	valuation 100 × $34,00 = $3 400,00 = 340_000 minor USD
	//	profit in USD = 340_000 − 300_000 =  +40_000 (+$400,00, +13,3 %)
	//	in rubles     = nothing whatsoever, on all four figures of the row, and
	//	                that IS the answer — see Handler.positionInBase, which
	//	                publishes no object rather than a basis summed from the
	//	                lots that happened to be datable.
	//
	// It is routed through Т-Банк because a transfer needs a source account
	// holding the instrument (see the Intel buy above). The source's real dates
	// are deliberately NOT carried across — that is exactly the case being
	// shown, and it is what the override branch does.
	handEnteredBasis := int64(300_000)
	if _, _, err := opSvc.CreateTransfer(ctx, spaceID, operation.TransferParams{
		FromAccountID: tbank, ToAccountID: freedom, InstrumentID: instIDs["INTC"],
		Quantity: decimal.RequireFromString("100"), OccurredOn: d("2026-07-20"),
		CostMinorOverride: &handEnteredBasis,
	}); err != nil {
		return fmt.Errorf("seed transfer INTC: %w", err)
	}

	// The two Amazon transfers, and the demo's pair for the journal's remaining
	// two sentences. One instrument, one day, two arrivals at Freedom KZ, and
	// they say different things — which is the point: the difference cannot come
	// from the ticker, the account or the date, so it has to come from the
	// purchases behind each amount.
	//
	// They are two transfers rather than one because a single transfer of both
	// parcels could only demonstrate the first of the two. A row's whole
	// base-currency block is published or withheld together, so a parcel
	// containing the undatable-in-rubles 11.05 lot withholds the figure for the
	// 12.06 lot as well, and the converted case would have nothing to stand on.
	//
	// Moving the parcels ONE AT A TIME is what the queue does anyway: each
	// transfer releases the earliest purchase still held (portfolio.ReleasedLots
	// — the same earliest-acquired-first rule everything else here obeys), so the
	// first call takes the 11.05 parcel and the second is left with the 12.06
	// one. Both are dated the same day as the three transfers above, and the
	// second sees the first because a transfer's basis is read from the journal
	// as it stood on its own date, same-date rows included (journalUpTo in
	// Service.CreateTransfer).
	//
	//	first  10 @ $180,00 bought 11.05.2026 -> 180_000 minor USD ($1 800,00)
	//	  in ₽ nothing: 11.05.2026 is before the fx history begins and there is
	//	       no earlier date to fall back to either, so BOTH LEGS publish no
	//	       ruble figure and name the cause as a missing rate for a PURCHASE
	//	       day — not for the transfer's own day, 20.07.2026, whose rate
	//	       (78.50) exists and is exactly the one that may not be used here.
	//	second 10 @ $200,00 bought 12.06.2026 -> 200_000 minor USD ($2 000,00)
	//	  in ₽ 200_000 × 81.00 (the rate of 11.06.2026, the last working day
	//	       before Russia Day) = 16_200_000 = 162 000,00 ₽
	//
	// The second row is where #80 is visible. Its tooltip's subject is a
	// purchase — «Это стоимость покупок … Самый поздний из них — на …» — so it
	// names dated_on, 12.06.2026, the day the shares were bought, while the rate
	// behind the figure is dated 11.06.2026. Every other transfer in this demo
	// moves lots whose own days all have rates, which makes the two dates equal
	// and the choice between them invisible.
	//
	// The second row's 162 000,00 ₽ is also the exact ruble figure of the BUY it
	// carries across (see that buy above, −162 000,00 ₽): same parcel, same
	// purchase day, same rate, once as money paid and once as basis moved. A
	// transfer valued at the day the paperwork moved would print 200_000 × 78.50
	// = 157 000,00 ₽ instead and disagree with a row four lines up its own
	// journal.
	//
	// At Freedom KZ the two parcels then make ONE position of 20 shares —
	// $3 800,00 of basis with no ruble figure at all, since one of its two lots
	// still cannot be dated to a rate. The journal row that can be converted and
	// the position that cannot are consistent: a position publishes its basis
	// whole or not at all, exactly as a journal row does.
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

	// THE DEMO'S LONG JOURNAL. Everything else in this seed is a scenario; these
	// 34 rows are not. They are ordinary housekeeping on the Т-Банк account —
	// a monthly top-up and the broker's monthly service charge — and their only
	// job is to make that account's journal longer than one page, so that
	// «Показать еще» is something the owner can click on the stand rather than
	// something only a test has ever seen (#86).
	//
	// WHY 34 AND NOT MORE. The client asks for 50 entries at a time
	// (JOURNAL_PAGE_SIZE, web/src/api/operations.ts). Т-Банк already holds 21
	// rows — sixteen operations above plus the five departing transfer legs — so
	// 34 puts it at 55: the first page comes back full with has_more, the button
	// appears, and the second page is five rows and ends. That is the smallest
	// addition that shows BOTH halves of the fix, the button appearing when there
	// is more and going away when there is not. A hundred rows would demonstrate
	// nothing further and would bury the four scenarios this branch is about.
	//
	// WHY THIS ACCOUNT. The button belongs under the journal the reader is
	// already on. Т-Банк carries both Amazon parcels, the Saturday deposit and
	// the bond whose price is money per unit, so all of this branch's material
	// and its paging are one screen.
	//
	// WHY THEY ARE DATED BEFORE EVERYTHING ELSE. The journal comes back newest
	// first, so rows dated 2024-12..2026-04 sort BEHIND every scenario row (which
	// start 2026-05-05). The first page opens on the 21 rows that mean something
	// and the housekeeping trails after them; the second page is housekeeping
	// alone. Dating them later would have pushed the scenarios onto page two.
	//
	// WHY THEY MOVE NO FIGURE ANYWHERE. They are rubles on a ruble account, so
	// nothing about them converts and none of them can carry a rate, a gap or a
	// caption. They name no instrument, so portfolio.Compute skips them before it
	// reaches a position (see its cash-level branch) and no basis, valuation or
	// realized result changes. And an account's balance is a figure the user
	// RECORDS: every balance on screen is read from account_balances
	// (account.Store.ListWithBalance and SummaryByCurrency), and this program
	// never sums a journal into a balance — so the three recorded balances above
	// stay exactly what they were.
	//
	// The 1st of the month is the anchor rather than the dates themselves, so
	// the 5th and the 28th exist in every month February included.
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
//     NVDA transfer call for the full arithmetic. 2026-06-20 does double duty
//     as the day the Alphabet parcel is SOLD, which is what values that
//     deal's proceeds (plan 7b — see the Alphabet buy): 65.00 against the
//     81.40 its expense was struck at is the whole reason a dollar profit
//     settles as a ruble loss there. 2026-06-20 is also the eurobond's buy
//     date, which is what its ruble basis is struck at (373 750,00 ₽).
//     2026-06-15 likewise does double duty as the Intel buy's date, though
//     nothing on screen converts through it: that parcel leaves for Freedom KZ
//     with a hand-typed basis and arrives with no purchase date at all, so the
//     rate reaches only the Т-Банк journal row for the buy itself.
//   - 2026-05-20 — the FXUS and WeWork buys' own date: converted at the exact
//     date's rate, the ordinary case.
//   - 2026-06-10 — the AAPL, MSFT and Alphabet buys' own date, at a visibly
//     different rate: two operations, two dates, two rates, so the journal
//     cannot be mistaken for "everything at today's rate". It is also the
//     highest rate in the set, and deliberately so: it is the day the MSFT lot
//     and the Alphabet parcel were bought, and an expense struck at 81.40
//     against proceeds struck lower is what turns a dollar profit into a ruble
//     loss — on paper for MSFT, valued at today's 78.50 (see the MSFT buy),
//     and for good for Alphabet, sold at 65.00 (see the Alphabet buy).
//   - 2026-06-11 — the Thursday before Russia Day, and the newest rate a
//     purchase made ON that holiday can reach. 12 июня is a non-working day in
//     Russia and the CBR sets no rate for it; the New York exchange trades, so
//     the second Amazon parcel is bought that day at a price the ruble has no
//     rate for. The pair is what makes «the day the figure is valued at» and
//     «the day its rate came from» two different days on the demo — on an
//     ordinary buy row and, more importantly, on the transfer that carries that
//     parcel, where the sentence's subject is the PURCHASE and naming the rate's
//     date instead was #80. 81.00 is close to 2026-06-10's 81.40 but not equal
//     to it, so a conversion that reached for the wrong neighbour would print a
//     different number rather than the same one.
//   - 2026-07-03 — the Friday before the Saturday USD deposit, which has no
//     rate of its own: the entry converts at the nearest earlier date and
//     the journal discloses that date rather than claiming the operation's
//     own. The same shape as 2026-06-11 above, on a weekend instead of a
//     holiday and on a deposit instead of a purchase.
//   - 2026-07-20 — today's-rate anchor, shared with the latest account
//     balances and with every seeded quote but WeWork's (see seedMarketData
//     for why that one is dated earlier); also what GET /summary converts at, and the
//     date of all three transfers (TSLA, NVDA and the hand-priced Intel
//     parcel). It is one half of the EUR->USD rate the eurobond's valuation
//     needs: no EUR/USD pair is seeded, and none is needed — every rate this
//     program stores is quoted against the ruble, so the converter bridges
//     92.30 ÷ 78.50 out of the two rows it already has (see
//     marketdata.resolveRate).
//
// Nothing at all is seeded before 2026-05-13, and three USD operations fall in
// that opening gap on purpose. A rate lookup resolves the nearest EARLIER date
// (marketdata.Store.FxRateOn), so these three — and only these three — have no
// rate to fall back on at all, and each buys a different demonstration:
//
//   - the Freedom KZ deposit (2026-05-06) has no resolvable rate, so that
//     journal entry must show its original amount with an explanation instead
//     of a dash or a zero;
//   - the first AAPL buy (2026-05-08) is one of two lots of a live position, so
//     that whole position must refuse to be shown in rubles rather than publish
//     a basis built from the one lot that did convert;
//   - the first Amazon buy (2026-05-11) is later moved to another account, so
//     the TRANSFER carrying it has to say the missing rate is one for a PURCHASE
//     day — while its own day, 2026-07-20, has a perfectly good rate that may
//     not be used (#79). The buy row itself sits three lines away saying the
//     other sentence, «нет курса на дату операции», about the same day.
//
// Seeding a rate for any of them would hide the honest-degradation paths behind
// unit tests where nobody looks at them. On an instance with internet access
// the fx backfill job eventually fills those dates from cbr.ru and all three
// start converting on their own — which is the point of the job.
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

// seedMarketData records the FX rates and instrument quotes the demo
// space's market valuation depends on: GET /summary's total_in_base_minor
// (base currency RUB, so USD/KZT need a rate to convert), GET
// .../positions' market_value_minor (share/bond positions need a quote to
// price), and the journal's per-operation in_base (each foreign-currency
// entry converted at the rate of the day it happened).
//
// The EUR/KZT rates are pinned to 2026-07-20 — the same date as the latest
// seeded account balance and the newest USD/RUB row — because that is this
// demo's "now": every conversion struck at the current rate resolves there.
//
// A QUOTE's date is a different kind of date and is written here as one. It is
// the trading SESSION the price belongs to, as stated by whoever published it
// (marketdata.TickerQuote.On) — never the day the price was fetched, which is
// what #90 was. So the dates below are weekdays a reader can take for
// sessions: 2026-07-20, a Monday, for all of them but WeWork's, whose earlier
// one is deliberate and explained at its quote. No arithmetic reads any of
// them. The one thing a quote's date decides is which of an instrument's rows
// is its latest (Store.LatestQuotes orders by on_date), and this function
// writes exactly one row per instrument; beyond that the date reaches a person
// only as the «Цена на …» caption on the positions screen.
//
// EUR/RUB is load-bearing rather than decorative: it is the rate
// the eurobond's euro-denominated valuation reaches the base currency by, and
// bridged against USD/RUB it is also the rate that brings that valuation into
// the position's dollars (see the KAZ32EUR buy). USD/RUB, in contrast, is seeded as a short HISTORY, with a
// deliberately different rate per date: the journal converts every entry at
// its own date's rate, and a single flat rate would make a historical
// conversion and a today's-rate conversion produce identical numbers,
// leaving the feature invisible on the demo screens. See seededUSDRates for
// what each date demonstrates.
//
// FXUS and AAPL deliberately get no quote, so their positions demonstrate the
// null-valuation path: a holding the app cannot price shows a dash and says
// so, rather than a silent zero. The cause in both cases is this function and
// nothing else — no quote row is written for either ticker below.
//
// It is NOT that FXUS is Frozen, which is what this comment claimed until
// #85. Nothing on the way to a valuation reads that flag: ListTradable selects
// by type and ticker alone (see internal/instrument/store.go), marketValue
// never sees the instrument's flags at all, and the only place the flag
// reaches a person is the «заморожен» badge on the positions screen — the
// instrument API carries the field both ways, but no screen sets it and no
// computation branches on it. Setting it on
// FXUS is therefore decoration for the demo — a delisted FinEx fund is what a
// reader expects to see priceless — and if the flag is to actually stop a
// lookup, that is a behaviour change owed its own argument, not something to
// be assumed by a comment.
//
// AAPL is priceless for a different and real reason: it is a live foreign
// instrument, and this program has no provider that covers one. The two
// wired in cmd/babki/root.go are cbr (fx rates) and moex (quotes), both
// Russian-market only.
//
// MSFT is the exception among the foreign instruments, and it is hand-seeded
// rather than provider-supplied for one reason: a position needs a valuation
// before its profit can differ in sign between its own currency and rubles,
// and that difference is the demo's whole reason for existing (see the MSFT
// buy for the arithmetic). 510.00 against a 500.00 purchase is a 2 % gain in
// dollars — small on purpose, because the ruble moved 3.7 % the other way
// over the same weeks and the point is that the smaller move loses.
//
// KAZ32EUR, WEWKQ, INTC and AMZN are hand-seeded for the same kind of reason —
// each carries a demonstration of its own that cannot be seen without a
// valuation. See each quote below.
func seedMarketData(ctx context.Context, pool *pgxpool.Pool, instIDs map[string]uuid.UUID, d func(string) time.Time) error {
	mdStore := marketdata.NewStore(pool)
	// The demo's "now": the newest fx rate in the table, so every conversion
	// struck at the current rate resolves here, and the same day as the newest
	// account balance.
	on := d("2026-07-20")
	// The trading session most of the seeded quotes belong to. The same
	// calendar day as the "now" above, and a separate name on purpose: a
	// quote's date says which session priced the paper, while `on` dates a rate
	// something is converted at. The two coincide here because it makes the
	// demo easy to read, not because anything requires it. 2026-07-20 is a
	// Monday, which is what lets a reader take it for a session at all.
	session := d("2026-07-20")
	// WeWork's own session: a Friday ten days earlier — see its quote below.
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

	// OFZ26238's price is a percentage of face value (95.20 meaning 95.20%),
	// same convention as a real bond quote — see portfolio.marketValue. Against
	// the 95,00 % this position was bought at, that is 952,00 ₽ a bond against
	// 950,00 ₽ paid; the OFZ buy spells the whole comparison out.
	quotes := []marketdata.Quote{
		{InstrumentID: instIDs["SBER"], On: session, Price: rate("305.50"), Currency: "RUB", Source: "seed"},
		{InstrumentID: instIDs["LKOH"], On: session, Price: rate("7550.00"), Currency: "RUB", Source: "seed"},
		{InstrumentID: instIDs["OFZ26238"], On: session, Price: rate("95.20"), Currency: "RUB", Source: "seed"},
		{InstrumentID: instIDs["MSFT"], On: session, Price: rate("510.00"), Currency: "USD", Source: "seed"},
		// NVDA is quoted at exactly the price the 2026-07-22 sale went off at,
		// so the surviving parcel is valued at $2 000.00 against a $1 500.00
		// basis: +$500.00 unrealized on the row itself, beside the +$1 000.00
		// that sale locked in — which reaches the screen through the account's
		// «Зафиксировано» line rather than the row, since the per-position
		// realized column was removed as visual noise (owner feedback, 4c).
		// Both figures are round and checkable without a calculator.
		{InstrumentID: instIDs["NVDA"], On: session, Price: rate("200.00"), Currency: "USD", Source: "seed"},
		// The eurobond, quoted like any bond as a percentage of face — 98.00
		// meaning 98,00 %. The `Currency` here is the unit that PERCENTAGE is
		// quoted in, not a currency the resulting money is ever in: the money is
		// denominated in the face value's currency, which is why marketValue
		// reads FaceCurrency and not this field (see its doc comment). It is
		// written as EUR rather than USD so the two say the same thing about
		// the same bond, the way OFZ26238's RUB quote does above.
		{InstrumentID: instIDs["KAZ32EUR"], On: session, Price: rate("98.00"), Currency: "EUR", Source: "seed"},
		// A price below a hundredth, and the only one in this seed: two fraction
		// digits would print it as «0,00» (#30). See the WEWKQ buy.
		//
		// It is also the one quote here dated to a DIFFERENT session from the
		// rest, and that is what puts plan 11's first half on the stand. A
		// quote's date belongs to the QUOTE — the session whoever published the
		// price says it priced — and not to the screen that draws it; a demo
		// where every row read the same day cannot tell those two apart, because
		// a single shared date is exactly what a screen-wide "as of today" stamp
		// looks like. With this row dated 2026-07-10 the positions screen shows
		// «Цена на 10.07.2026» beside «Цена на 20.07.2026» on its neighbours,
		// and the caption is visibly about the price rather than about the page.
		//
		// This row rather than SBER's or the OFZ's, because MOEX dates a whole
		// BOARD's session at once — every one of TQBR's 502 rows carried the
		// same PREVDATE in the measurement recorded in QuotesFor
		// (internal/marketdata/moex/moex.go), traded and untraded alike — so
		// splitting the Russian instruments apart would put a shape on the demo
		// that the provider it imitates does not produce. WeWork has no provider
		// at all here (nothing in this program covers a foreign market), and
		// what a stale row means is not in doubt either way: an instrument a
		// refresh cannot price keeps the row it already has, date included, since
		// quotesWorker.Work stores nothing for a ticker with no price and
		// deletes nothing.
		//
		// No figure on any screen moves because of this. Nothing computes from a
		// quote's date: the valuation is struck from the price, and its
		// conversion into rubles from today's rate — which is the second
		// sentence the row's own tooltip now carries.
		{InstrumentID: instIDs["WEWKQ"], On: weworkSession, Price: rate("0.0025"), Currency: "USD", Source: "seed"},
		// Intel is quoted so that the row carrying the dateless parcel has all
		// four money figures to withhold, not just two: a position with no quote
		// publishes no valuation and no profit anyway, and would show the
		// permanent «дату уже не восстановить» sentence on half the row it is
		// true of. See the INTC transfer.
		{InstrumentID: instIDs["INTC"], On: session, Price: rate("34.00"), Currency: "USD", Source: "seed"},
		// Amazon is quoted so its position reads as a position rather than as a
		// row of dashes. The two parcels moved in by the transfers cost
		// $3 800,00 together (180_000 + 200_000 minor), and 20 × $209,00 =
		// $4 180,00 values them at exactly ten percent more: +$380,00, +10,0 %.
		//
		// In rubles the same row publishes NOTHING — no basis, no valuation, no
		// profit — because one of its two lots was bought on 2026-05-11, a day
		// the demo's fx table cannot reach (see seededUSDRates). That is the
		// position's half of what the journal says two rows at a time: the row's
		// base-currency block goes whole or not at all, so the datable lot's
		// 162 000,00 ₽ does not appear on its own here either.
		{InstrumentID: instIDs["AMZN"], On: session, Price: rate("209.00"), Currency: "USD", Source: "seed"},
	}
	if err := mdStore.UpsertQuotes(ctx, quotes); err != nil {
		return fmt.Errorf("seed quotes: %w", err)
	}
	return nil
}
