package background

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/importer/tinvest"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/notify"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/payouts"
	"babki.my/babki/internal/platform/testdb"
)

// Decision Р-61 Б, over the base: a connection whose token the broker
// refused; the central bank's rates not loaded for ten days; a dividend that
// came yesterday to Alex's account; another declared with its record day
// ahead — each a reminder of its kind, the account's ones Alex's alone.
func TestTheDataEventsOverTheBase(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	fam := family.NewStore(pool)
	alex, err := fam.CreateUser(ctx, "alex", "Александр", "hash")
	if err != nil {
		t.Fatal(err)
	}
	space, err := fam.CreateSpaceWithOwner(ctx, "Семья", alex.ID)
	if err != nil {
		t.Fatal(err)
	}
	tv := tinvest.NewStore(pool)
	if _, err := tv.CreateConnection(ctx, space.ID, []byte("sealed"), "a1b2", tinvest.StatusTokenRevoked); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO job_outcomes (kind, last_success_at) VALUES ($1, $2)`,
		marketdata.RefreshFxArgs{}.Kind(), now.AddDate(0, 0, -10)); err != nil {
		t.Fatal(err)
	}
	accounts, instruments, ops, md := account.NewStore(pool), instrument.NewStore(pool), operation.NewStore(pool), marketdata.NewStore(pool)
	acc, err := accounts.Create(ctx, space.ID, &alex.ID, "Брокерский", account.TypeBrokerage, "RUB", "")
	if err != nil {
		t.Fatal(err)
	}
	sber, err := instruments.Create(ctx, instrument.Instrument{Type: instrument.TypeShare, Name: "Сбербанк", Ticker: "SBER", Currency: "RUB"})
	if err != nil {
		t.Fatal(err)
	}
	qty, price := decimal.NewFromInt(100), decimal.NewFromInt(300)
	for _, op := range []operation.Operation{
		{AccountID: acc.ID, InstrumentID: &sber.ID, Type: operation.TypeBuy, OccurredOn: today.AddDate(0, 0, -60), Quantity: &qty, Price: &price, AmountMinor: -30_000_00, Currency: "RUB"},
		{AccountID: acc.ID, InstrumentID: &sber.ID, Type: operation.TypeDividend, OccurredOn: today.AddDate(0, 0, -1), AmountMinor: 3_031_08, Currency: "RUB"},
	} {
		if _, err := ops.Create(ctx, space.ID, op, nil); err != nil {
			t.Fatal(err)
		}
	}
	paid, pays := today.AddDate(0, 0, -2), today.AddDate(0, 0, 35)
	if err := md.ReplaceDividends(ctx, sber.ID, "test", []marketdata.Dividend{
		{InstrumentID: sber.ID, Source: "test", RecordDate: today.AddDate(0, 0, -10), PaymentDate: &paid, PerShare: decimal.RequireFromString("34.84"), Currency: "RUB"},
		{InstrumentID: sber.ID, Source: "test", RecordDate: today.AddDate(0, 0, 20), PaymentDate: &pays, PerShare: decimal.RequireFromString("36.5"), Currency: "RUB"},
	}, now); err != nil {
		t.Fatal(err)
	}
	e := &events{
		pool: pool, tinvest: tv, accounts: accounts, instruments: instruments,
		payouts: payouts.NewService(ops, accounts, md, fam, marketdata.NewConverter(md)),
	}
	got, err := e.Reminders(ctx, space.ID, today, now)
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[notify.Kind]notify.Reminder{}
	var kinds []string
	for _, r := range got {
		byKind[r.Kind] = r
		kinds = append(kinds, string(r.Kind))
	}
	sort.Strings(kinds)
	if strings.Join(kinds, ",") != "declared,payout,stale,sync" {
		t.Fatalf("kinds = %v: %+v", kinds, got)
	}
	if r := byKind[notify.KindSync]; !strings.Contains(r.Body, "…a1b2") {
		t.Errorf("sync = %+v", r)
	}
	if r := byKind[notify.KindStale]; !strings.HasPrefix(r.Body, "Курсы валют ЦБ: последний раз") {
		t.Errorf("stale = %+v", r)
	}
	if r := byKind[notify.KindPayout]; r.Owner == nil || *r.Owner != alex.ID || !strings.HasPrefix(r.Body, "Сбербанк — дивиденды 3") {
		t.Errorf("payout = %+v", r)
	}
	if r := byKind[notify.KindDeclared]; r.Owner == nil || *r.Owner != alex.ID || r.Title != "Дивиденды: Сбербанк" || !strings.HasPrefix(r.Body, "36,5") {
		t.Errorf("declared = %+v", r)
	}
}
