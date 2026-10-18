package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// plain is a push's text with the no-break spaces of its sums as spaces.
func plain(s string) string { return strings.ReplaceAll(s, "\u00a0", " ") }

func on(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

// Decision Р-61 Б: a token the broker refused is told at once, runs failing
// only after two days; an account apart from its broker once a
// reconciliation; a source that stopped once for its last success.
func TestTheDataReminders(t *testing.T) {
	today := on("2026-10-19")
	ok, yesterday := on("2026-10-16"), on("2026-10-18")
	sync := SyncReminders([]SyncState{
		{ID: uuid.New(), Broker: "Т-Инвестиции", Last4: "a1b2", Revoked: true, Since: on("2026-10-18")},
		{ID: uuid.New(), Broker: "Т-Инвестиции", Failing: true, LastOK: &ok},
		{ID: uuid.New(), Broker: "Т-Инвестиции", Failing: true, LastOK: &yesterday},
	}, today)
	if len(sync) != 2 || !strings.Contains(sync[0].Body, "…a1b2") || !strings.Contains(sync[1].Body, "с 16.10") || sync[1].Kind != KindSync {
		t.Errorf("sync = %+v", sync)
	}
	owner := uuid.New()
	rec := ReconcileReminders([]Mismatch{{AccountID: uuid.New(), Name: "Брокерский", Owner: &owner, RunID: uuid.New(), On: yesterday}})
	if len(rec) != 1 || rec[0].Owner != &owner || !strings.Contains(rec[0].Body, "18.10") || !strings.HasPrefix(rec[0].Key, "reconcile:") {
		t.Errorf("reconcile = %+v", rec)
	}
	stale := StaleReminders([]StaleSource{{Kind: "fx", Name: "Курсы валют ЦБ", LastOK: &ok}, {Kind: "crypto", Name: "Цены криптовалют"}})
	if len(stale) != 2 || stale[0].Key != "stale:fx:2026-10-16" || stale[1].Key != "stale:crypto:never" || !strings.Contains(stale[0].Body, "последний раз 16.10") {
		t.Errorf("stale = %+v", stale)
	}
}

// Payouts that came in the last three days, one push a day for each member
// they are for; older ones and nothing at all are not told. A declared
// dividend whose record day is ahead, with what will come.
func TestThePayoutReminders(t *testing.T) {
	today := on("2026-10-19")
	alex := uuid.New()
	list := []Arrived{
		{AccountID: uuid.New(), Owner: &alex, Paper: "Сбербанк", What: "дивиденды", Got: 1_234_56, Currency: "RUB", On: on("2026-10-18")},
		{AccountID: uuid.New(), Owner: &alex, Paper: "ОФЗ 26238", What: "купон", Got: 345_67, Currency: "RUB", On: on("2026-10-18")},
		{AccountID: uuid.New(), Paper: "Лукойл", What: "дивиденды", Got: 500_00, Currency: "RUB", On: on("2026-10-17")},
		{AccountID: uuid.New(), Paper: "Старое", What: "купон", Got: 100_00, Currency: "RUB", On: on("2026-10-10")},
		{AccountID: uuid.New(), Paper: "Ноль", What: "купон", Got: 0, Currency: "RUB", On: on("2026-10-18")},
	}
	got := PayoutReminders(list, today)
	if len(got) != 2 {
		t.Fatalf("payouts = %+v", got)
	}
	if got[0].Owner != nil || plain(got[0].Body) != "Лукойл — дивиденды 500,00 ₽." {
		t.Errorf("the shared account's = %+v", got[0])
	}
	if *got[1].Owner != alex || plain(got[1].Body) != "Сбербанк — дивиденды 1 234,56 ₽; ОФЗ 26238 — купон 345,67 ₽." || got[1].Title != "Пришли выплаты 18.10" {
		t.Errorf("alex's = %+v", got[1])
	}
	net, pay := int64(30_310_80), on("2026-11-05")
	declared := DeclaredReminders([]Declared{
		{InstrumentID: uuid.New(), Paper: "Сбербанк", PerShare: decimal.RequireFromString("34.84"), Currency: "RUB", RecordOn: on("2026-10-25"), PayOn: &pay, Net: &net, NetCurrency: "RUB"},
		{InstrumentID: uuid.New(), Paper: "Прошлый", PerShare: decimal.NewFromInt(1), Currency: "RUB", RecordOn: on("2026-10-01")},
	}, today)
	if len(declared) != 1 || plain(declared[0].Body) != "34,84 ₽ на акцию, реестр 25.10.2026, выплата до 05.11.2026. Придёт ≈ 30 310,80 ₽." || declared[0].Kind != KindDeclared {
		t.Errorf("declared = %+v", declared)
	}
}

// Quiet hours over midnight, within a day, and none at all.
func TestQuietHours(t *testing.T) {
	night, lunch, never := Prefs{QuietFrom: 21, QuietTo: 9}, Prefs{QuietFrom: 13, QuietTo: 14}, Prefs{QuietFrom: 8, QuietTo: 8}
	for _, c := range []struct {
		p     Prefs
		hour  int
		quiet bool
	}{{night, 21, true}, {night, 3, true}, {night, 9, false}, {night, 20, false}, {lunch, 13, true}, {lunch, 14, false}, {never, 8, false}} {
		if c.p.Quiet(c.hour) != c.quiet {
			t.Errorf("%+v at %d: quiet %v", c.p, c.hour, !c.quiet)
		}
	}
}

// Decision Р-58: a card whose period has no picks yet is reminded once.
func TestTheLoyaltyReminder(t *testing.T) {
	owner := uuid.New()
	got := LoyaltyReminders([]PickDue{{AccountID: uuid.New(), Name: "Карта ВТБ", Owner: &owner, From: on("2026-11-01"), To: on("2026-11-30")}})
	if len(got) != 1 || got[0].Kind != KindLoyalty || !strings.HasSuffix(got[0].Key, ":2026-11-01") || !strings.Contains(got[0].Body, "с 01.11 по 30.11") || got[0].Owner != &owner {
		t.Errorf("reminder = %+v", got)
	}
}
