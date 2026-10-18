package deposit

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/shopspring/decimal"
)

func day(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

// keys is a key rate history: a rate from each day on.
func keys(points ...any) KeyOn {
	return func(on time.Time) (decimal.Decimal, bool, error) {
		var out decimal.Decimal
		found := false
		for i := 0; i+1 < len(points); i += 2 {
			if !day(points[i].(string)).After(on) {
				out, found = decimal.RequireFromString(points[i+1].(string)), true
			}
		}
		return out, found, nil
	}
}

// marks are the conditions met: an id to its level, in every month.
func marks(m map[string]int) Conditions {
	return func(id string, _ time.Time) (int, bool) {
		level, ok := m[id]
		return level, ok
	}
}

func programme(t *testing.T, id string) Programme {
	t.Helper()
	p, ok := ByID(id)
	if !ok {
		t.Fatalf("no %s in the catalog", id)
	}
	return p
}

func pct(t *testing.T, q Quote, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if !q.Percent.Equal(decimal.RequireFromString(want)) {
		t.Errorf("rate = %s, want %s (parts %+v)", q.Percent, want, q.Parts)
	}
}

// Every programme of the catalog reads and holds together.
func TestTheCatalogHolds(t *testing.T) {
	all, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 4 {
		t.Fatalf("catalog = %d programmes", len(all))
	}
	for _, p := range all {
		if len(p.Watch) == 0 {
			t.Errorf("%s: no pages to watch", p.ID)
		}
	}
}

// A file that would mislead the rate is refused: no bank, a grid that is
// neither fixed nor tied, a deposit's cell without its term, editions out of
// order, a bonus of an unknown check.
func TestABrokenProgrammeIsRefused(t *testing.T) {
	good := `
id: x
bank: vtb
name: X
kind: deposit
editions:
  - from: 2026-01-01
    checked_on: 2026-01-02
    sources: [https://example.ru]
    variants:
      - { id: a, audience: [all], payouts: [end], ops: none, term_unit: days, grid: [{ from: 1, to: 30, percent: "10" }] }
`
	if _, err := readCatalog(fstest.MapFS{"catalog/x.yaml": {Data: []byte(good)}}); err != nil {
		t.Fatalf("a good file: %v", err)
	}
	order := good + `  - from: 2025-01-01
    checked_on: 2026-01-02
    sources: [https://example.ru]
    variants:
      - { id: a, audience: [all], payouts: [end], ops: none, term_unit: days, grid: [{ from: 1, to: 30, percent: "10" }] }
`
	for name, text := range map[string]string{
		"no bank":       replace(good, "bank: vtb", "bank: nobank"),
		"neither":       replace(good, `percent: "10"`, `amount_from: 0`),
		"no term":       replace(good, "from: 1, to: 30, ", ""),
		"order":         order,
		"unknown check": replace(good, "grid:", `bonuses: [{ id: b, plus: "1", check: guess }], grid:`),
		"no plus":       replace(good, "grid:", `bonuses: [{ id: b, check: manual, terms: [{ from: 1, to: 30 }] }], grid:`),
		"terms upside":  replace(good, "grid:", `bonuses: [{ id: b, plus: "1", check: manual, terms: [{ from: 30, to: 1 }] }], grid:`),
		"currency":      replace(good, "grid:", `currency: yuan, grid:`),
		"no audience":   replace(good, "audience: [all], ", ""),
		"all and more":  replace(good, "audience: [all]", "audience: [all, pensioner]"),
		"who":           replace(good, "audience: [all]", "audience: [martians]"),
	} {
		if _, err := readCatalog(fstest.MapFS{"catalog/x.yaml": {Data: []byte(text)}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func replace(s, old, new string) string {
	if !strings.Contains(s, old) {
		panic("no " + old)
	}
	return strings.Replace(s, old, new, 1)
}

// The note's family (Р-53): the ВТБ-Счёт on the minimal balance. A new
// client has 14,2 % for the month of opening and the next — up to 1 млн,
// the base 6 % above it, no bonuses; then 6 + 2 for the wages (a pension at
// the same time adds nothing more) + 4 for purchases from 50 000 ₽ = 12 %,
// on up to 1 млн. Not a new client — the base and the bonuses at once.
func TestTheVTBSavingsAccount(t *testing.T) {
	p := programme(t, "vtb-schet")
	h := Holding{Variant: "minimum", OpenedOn: day("2026-10-16")}
	all := marks(map[string]int{"new_client": 0, "salary": 0, "pension": 0, "spend": 1})
	q, err := p.Quote(h, day("2026-11-30"), 900_000_00, nil, all)
	pct(t, q, err, "14.2")
	if q.Period != "Приветственная" || len(q.Bonuses) != 0 {
		t.Errorf("welcome = %+v", q)
	}
	q, err = p.Quote(h, day("2026-11-30"), 1_200_000_00, nil, all)
	pct(t, q, err, "12.833333") // (14,2 × 1 млн + 6 × 200 000) / 1,2 млн
	q, err = p.Quote(h, day("2026-12-01"), 1_200_000_00, nil, all)
	pct(t, q, err, "11") // (12 × 1 млн + 6 × 200 000) / 1,2 млн
	if len(q.Bonuses) != 2 {
		t.Errorf("bonuses = %v, want one of wages and pension and purchases", q.Bonuses)
	}
	q, err = p.Quote(h, day("2026-10-20"), 500_000_00, nil, marks(map[string]int{"subscription": 0, "level": 1}))
	pct(t, q, err, "9.5") // 6 + 2 + 1,5: no welcome
	daily := Holding{Variant: "daily", OpenedOn: day("2026-10-16")}
	q, err = p.Quote(daily, day("2026-10-20"), 500_000_00, nil, all)
	pct(t, q, err, "13.2")
}

// Газпромбанк's «Ключевой момент» for 367 days: the key rate less 0,2. The
// key rate cut on a Friday counts from Monday; a term the bank has no rate
// for, or a deposit opened before the edition, gets none.
func TestTheKeyMomentFollowsTheKeyRateFromTheNextWorkingDay(t *testing.T) {
	p := programme(t, "gazprombank-klyuchevoy-moment")
	h := Holding{Variant: "end", OpenedOn: day("2026-10-05"), Term: 367, Amount: 500_000_00}
	key := keys("2026-07-27", "14", "2026-10-23", "13")
	q, err := p.Quote(h, day("2026-10-23"), 500_000_00, key, nil)
	pct(t, q, err, "13.8")
	q, err = p.Quote(h, day("2026-10-25"), 500_000_00, key, nil)
	pct(t, q, err, "13.8")
	q, err = p.Quote(h, day("2026-10-26"), 500_000_00, key, nil)
	pct(t, q, err, "12.8")
	h.Term = 366
	if _, err := p.Quote(h, day("2026-10-26"), 500_000_00, key, nil); !errors.Is(err, ErrNoCell) {
		t.Errorf("366 days: %v", err)
	}
	h.Term, h.OpenedOn = 367, day("2026-10-02")
	if _, err := p.Quote(h, day("2026-10-26"), 500_000_00, key, nil); !errors.Is(err, ErrNoEdition) {
		t.Errorf("opened before the edition: %v", err)
	}
	if _, err := p.Quote(Holding{Variant: "end", OpenedOn: day("2026-10-05"), Term: 367, Amount: 500_000_00}, day("2026-10-26"), 1, keys(), nil); !errors.Is(err, ErrNoKeyRate) {
		t.Errorf("no key rate: %v", err)
	}
}

// Т-Банк's СмартВклад: a fixed rate by the term's months, the sum from
// 50 000 ₽; no rate for less, nor for a 1 month deposit topped up.
func TestTheSmartDepositByMonths(t *testing.T) {
	p := programme(t, "tbank-smartvklad")
	q, err := p.Quote(Holding{Variant: "fixed", OpenedOn: day("2026-10-01"), Term: 6, Amount: 100_000_00}, day("2027-01-15"), 100_000_00, nil, nil)
	pct(t, q, err, "11.5")
	for _, h := range []Holding{
		{Variant: "topup", OpenedOn: day("2026-10-01"), Term: 1, Amount: 100_000_00},
		{Variant: "fixed", OpenedOn: day("2026-10-01"), Term: 6, Amount: 40_000_00},
	} {
		if _, err := p.Quote(h, day("2026-10-15"), h.Amount, nil, nil); !errors.Is(err, ErrNoCell) {
			t.Errorf("%+v: %v", h, err)
		}
	}
	if _, err := p.Quote(Holding{Variant: "nope", OpenedOn: day("2026-10-01"), Term: 6, Amount: 100_000_00}, day("2026-10-15"), 1, nil, nil); !errors.Is(err, ErrNoVariant) {
		t.Errorf("no variant: %v", err)
	}
}

// Кубань Кредит's savings account: for two calendar months the key rate on
// up to 1 000 000 ₽ and 0,6 of it above, the bonuses not added; then 0,6 of
// the key rate on all with 3 for the wages and 1 for purchases from 50 000 ₽.
func TestTheKubanSavingsAccountSplitsTheBalance(t *testing.T) {
	p := programme(t, "kubankredit-nakopitelniy")
	h := Holding{Variant: "daily", OpenedOn: day("2026-10-20")}
	key := keys("2026-07-27", "14")
	met := marks(map[string]int{"income": 0, "spend": 1})
	q, err := p.Quote(h, day("2026-11-10"), 1_500_000_00, key, met)
	// (14 × 100 000 001 + 8,4 × 49 999 999) / 150 000 000, in kopecks.
	pct(t, q, err, "12.133333")
	q, err = p.Quote(h, day("2026-12-01"), 1_500_000_00, key, met)
	pct(t, q, err, "12.4")
	q, err = p.Quote(h, day("2026-12-01"), 0, key, nil)
	pct(t, q, err, "8.4")
}

// A new key rate by the month (Новикомбанк, «Рантье»): a change up to the
// 25th counts from the first working day of the next month, a later one from
// its fifth.
func TestTheKeyRateByTheMonth(t *testing.T) {
	lag := &KeyLag{Cutoff: 25, Workday: 1, LateWorkday: 5}
	for on, want := range map[string]string{
		"2026-11-01": "2026-09-30", // a Sunday before the month's first working day
		"2026-11-02": "2026-10-25", // the first working day: changes up to 25 October
		"2026-11-06": "2026-10-25",
		"2026-11-09": "2026-10-31", // the fifth working day (4 November is a holiday)
	} {
		if got := keyDay(day(on), lag).Format(time.DateOnly); got != want {
			t.Errorf("%s: key of %s, want %s", on, got, want)
		}
	}
}

// A deposit of a part above a cap at a share of the rate (ПСБ: 0,65 of it
// above 20 млн), a bonus of one of a group, a welcome period by days.
func TestCapsAndGroups(t *testing.T) {
	p := Programme{ID: "t", Kind: KindDeposit, Editions: []Edition{{From: "2026-01-01", Variants: []Variant{{
		ID: "a", TermUnit: TermDays, Grid: []Cell{{From: 1, To: 400, Rate: Rate{Percent: "10"}}},
		Cap: &Cap{Amount: 20_000_000_00, Share: "0.65"},
		Bonuses: []Bonus{
			{ID: "x", Plus: "1", Group: "g"},
			{ID: "y", Plus: "2", Group: "g"},
		},
		Schedule: []Period{{Name: "w", Days: 62, Plus: "1.5", Bonuses: true}},
	}}}}}
	h := Holding{Variant: "a", OpenedOn: day("2026-03-01"), Term: 370, Amount: 30_000_000_00}
	q, err := p.Quote(h, day("2026-06-01"), 30_000_000_00, nil, marks(map[string]int{"x": 0, "y": 0}))
	pct(t, q, err, "10.6") // (12 × 20 млн + 12 × 0,65 × 10 млн) / 30 млн: the group's greater bonus
	q, err = p.Quote(h, day("2026-04-30"), 10_000_000_00, nil, nil)
	pct(t, q, err, "11.5") // day 61 of the 62
}

// ВТБ's way: the wages' bonus is +1 for 61 days and +0,4 for three years;
// the level's is one for every term; a bonus for terms up to a year only
// gives a deposit of three years nothing.
func TestABonusByTheTerm(t *testing.T) {
	p := Programme{ID: "t", Kind: KindDeposit, Editions: []Edition{{From: "2026-01-01", Variants: []Variant{{
		ID: "a", TermUnit: TermDays, Grid: []Cell{{From: 61, To: 61, Rate: Rate{Percent: "11"}}, {From: 1095, To: 1095, Rate: Rate{Percent: "9.8"}}},
		Bonuses: []Bonus{
			{ID: "wages", Terms: []TermPlus{{From: 61, To: 61, Plus: "1"}, {From: 1095, To: 1095, Plus: "0.4"}}},
			{ID: "level", Levels: []Level{{From: 1, Plus: "0.25"}, {From: 2, Plus: "0.5"}}},
			{ID: "short", Plus: "0.5", Terms: []TermPlus{{From: 1, To: 370}}},
		},
	}}}}}
	met := marks(map[string]int{"wages": 0, "level": 0, "short": 0})
	q, err := p.Quote(Holding{Variant: "a", OpenedOn: day("2026-03-01"), Term: 61, Amount: 1_000_000_00}, day("2026-03-10"), 1_000_000_00, nil, met)
	pct(t, q, err, "12.75")
	q, err = p.Quote(Holding{Variant: "a", OpenedOn: day("2026-03-01"), Term: 1095, Amount: 1_000_000_00}, day("2026-03-10"), 1_000_000_00, nil, met)
	pct(t, q, err, "10.45")
	if len(q.Bonuses) != 2 {
		t.Errorf("bonuses of three years = %v", q.Bonuses)
	}
}

// The catalog against the banks' own figures, 16.10.2026, key rate 14 %.
func TestTheBanksOwnFigures(t *testing.T) {
	key := keys("2026-07-24", "14")
	cases := []struct {
		name, programme string
		h               Holding
		day             string
		balance         int64
		met             map[string]int
		want            string
	}{
		// ВТБ: «Уже получаете зарплату», ВТБ Онлайн, в конце срока, 370 дней, от 500 000 ₽ — 12,60 %.
		{"ВТБ, зарплата", "vtb-vklad", Holding{Variant: "online-end", OpenedOn: day("2026-10-16"), Term: 370, Amount: 600_000_00}, "2026-10-20", 600_000_00, map[string]int{"salary": 0}, "12.6"},
		// ВТБ: Серебряный уровень и подписка, 1095 дней, от 1,5 млн — 9,80 + 0,25 + 0,2.
		{"ВТБ, уровень и подписка", "vtb-vklad", Holding{Variant: "online-end", OpenedOn: day("2026-10-16"), Term: 1095, Amount: 2_000_000_00}, "2026-11-20", 2_000_000_00, map[string]int{"subscription": 0, "level": 0}, "10.25"},
		// ВТБ «Ключевой»: в конце срока, 1095 дней, от 500 000 ₽ — КС + 0,30 = 14,30 %.
		{"ВТБ Ключевой", "vtb-klyuchevoy", Holding{Variant: "end", OpenedOn: day("2026-10-16"), Term: 1095, Amount: 600_000_00}, "2026-10-20", 600_000_00, nil, "14.3"},
		// Сбер: «максимальные 13,5 %» — 3–7 месяцев, в конце срока, от 1 млн, все надбавки.
		{"СберВклад, все надбавки", "sber-sbervklad", Holding{Variant: "end", OpenedOn: day("2026-10-16"), Term: 6, Amount: 1_200_000_00}, "2026-10-20", 1_200_000_00, map[string]int{"prime": 0, "salary": 0, "savings": 0}, "13.5"},
		// Сбер: у вкладов от 1,5 лет надбавок нет.
		{"СберВклад, 1,5 года", "sber-sbervklad", Holding{Variant: "end", OpenedOn: day("2026-10-16"), Term: 18, Amount: 1_200_000_00}, "2026-10-20", 1_200_000_00, map[string]int{"prime": 0, "salary": 0, "savings": 0}, "8.5"},
		// Сбер «Ключевой»: ключевая − 2.
		{"Сбер Ключевой", "sber-klyuchevoy", Holding{Variant: "monthly", OpenedOn: day("2026-10-16"), Term: 12, Amount: 500_000_00}, "2026-10-20", 500_000_00, nil, "12"},
		// Сбер, накопительный: первые два месяца 6,5 + 6 = 12,5 %.
		{"Сбер, приветственная", "sber-nakopitelny", Holding{Variant: "daily", OpenedOn: day("2026-10-01")}, "2026-10-20", 300_000_00, map[string]int{"new_client": 0}, "12.5"},
		// Потом «до 12,5 % с надбавками»: покупки от 50 000 ₽ +3,5, партнёры +1,5, СберПрайм +1.
		{"Сбер, надбавки", "sber-nakopitelny", Holding{Variant: "daily", OpenedOn: day("2026-01-10")}, "2026-10-20", 300_000_00, map[string]int{"spend": 1, "partners": 0, "prime": 0}, "12.5"},
		// Т-Банк: с Premium — 9 %.
		{"Т-Банк, Premium", "tbank-nakopitelny", Holding{Variant: "daily", OpenedOn: day("2026-01-10")}, "2026-10-20", 300_000_00, map[string]int{"subscription": 1}, "9"},
		// Газпромбанк: на минимальный остаток, приветственная 14 %, потом 12,75 %.
		{"ГПБ, приветственная", "gazprombank-gpb-schet", Holding{Variant: "minimum", OpenedOn: day("2026-10-02")}, "2026-11-20", 300_000_00, map[string]int{"new_client": 0}, "14"},
		{"ГПБ, база", "gazprombank-gpb-schet", Holding{Variant: "minimum", OpenedOn: day("2026-10-02")}, "2026-12-01", 300_000_00, map[string]int{"new_client": 0}, "12.75"},
		// «Копить», онлайн, 181 день, с надбавкой за зарплату — максимальные 12,8 %.
		{"ГПБ Копить", "gazprombank-kopit", Holding{Variant: "online", OpenedOn: day("2026-10-16"), Term: 181, Amount: 100_000_00}, "2026-10-20", 100_000_00, map[string]int{"salary": 0, "pension": 0}, "12.8"},
		// Альфа: покупки от 100 000 ₽ — 12 % на сумму до 1 млн, сверх — базовые 7 %.
		{"Альфа, покупки", "alfa-schet", Holding{Variant: "minimum", OpenedOn: day("2026-07-10")}, "2026-10-20", 2_000_000_00, map[string]int{"spend": 1}, "9.5"},
		// Россельхозбанк: «до 13,5 %» — 4 месяца дистанционно, новый сберегатель.
		{"РСХБ, новый сберегатель", "rshb-svoy-vklad", Holding{Variant: "remote-end", OpenedOn: day("2026-10-16"), Term: 4, Amount: 100_000_00}, "2026-10-20", 100_000_00, map[string]int{"new_saver": 0}, "13.5"},
		// «Мой счёт»: покупки от 90 000 ₽ — 6 + 7.
		{"РСХБ, Мой счёт", "rshb-moy-schet", Holding{Variant: "daily", OpenedOn: day("2026-01-10")}, "2026-10-20", 500_000_00, map[string]int{"spend": 2}, "13"},
		// ДОМ.РФ «Мой Дом», 91 день, от 1,5 млн, «новые деньги» — 13,80 % / 14,50 %.
		{"ДОМ.РФ, новые деньги", "domrf-moy-dom", Holding{Variant: "none-end", OpenedOn: day("2026-10-16"), Term: 91, Amount: 2_000_000_00}, "2026-10-20", 2_000_000_00, map[string]int{"new_money": 0}, "14.5"},
		// «Активный процент», 91 день: в месяц открытия — наибольшая, потом по покупкам прошлого месяца.
		{"ДОМ.РФ, месяц открытия", "domrf-aktivny-procent", Holding{Variant: "d91", OpenedOn: day("2026-10-16"), Term: 91, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "14.5"},
		{"ДОМ.РФ, покупки 55 000", "domrf-aktivny-procent", Holding{Variant: "d91", OpenedOn: day("2026-10-16"), Term: 91, Amount: 100_000_00}, "2026-11-20", 100_000_00, map[string]int{"spend": 1}, "13.9"},
		// БСПБ: «Максимальный online», 181 день, в конце срока — 12,9 %; накопительный с покупками — 12,5 %.
		{"БСПБ, Максимальный", "bspb-maksimalny", Holding{Variant: "online-end", OpenedOn: day("2026-10-16"), Term: 181, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "12.9"},
		{"БСПБ, накопительный", "bspb-nakopitelny", Holding{Variant: "minimum", OpenedOn: day("2026-01-10")}, "2026-10-20", 500_000_00, map[string]int{"spend": 0}, "12.5"},
		// Уралсиб «Ключевой Плюс», 91 день — 13,20 % при ключевой 14.
		{"Уралсиб, Ключевой Плюс", "uralsib-klyuchevoy-plus", Holding{Variant: "monthly", OpenedOn: day("2026-10-16"), Term: 91, Amount: 600_000_00}, "2026-10-20", 600_000_00, nil, "13.2"},
		// Банк «Россия»: интернет-банк, 181 день, от 1 млн — 12,50 %.
		{"Россия", "rossiya-rossiya", Holding{Variant: "online", OpenedOn: day("2026-10-16"), Term: 181, Amount: 1_500_000_00}, "2026-10-20", 1_500_000_00, nil, "12.5"},
		// ПСБ: «Мой доход» в офисе, 181 день — 14,10 %; «Моя копилка» 181 день, 25 млн — 11,7 на 20 млн и 0,65 ставки сверх.
		{"ПСБ, Мой доход", "psb-moy-dohod", Holding{Variant: "office", OpenedOn: day("2026-10-16"), Term: 181, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "14.1"},
		{"ПСБ, Моя копилка", "psb-moya-kopilka", Holding{Variant: "monthly", OpenedOn: day("2026-10-16"), Term: 181, Amount: 25_000_000_00}, "2026-10-20", 25_000_000_00, nil, "10.881"},
		// «Акцент на процент»: покупки от 100 000 ₽ — 14 %.
		{"ПСБ, Акцент", "psb-akcent", Holding{Variant: "daily", OpenedOn: day("2026-01-10")}, "2026-10-20", 500_000_00, map[string]int{"spend": 2}, "14"},
		{"ПСБ, Про запас", "psb-pro-zapas", Holding{Variant: "minimum", OpenedOn: day("2026-10-10")}, "2026-10-20", 500_000_00, map[string]int{"new_client": 0}, "13"},
		// Ozon: непополняемый на 3 месяца — номинальные 13,55 % (в рекламе «с капитализацией» 13,7 %).
		{"Ozon, вклад", "ozon-vklad", Holding{Variant: "fixed", OpenedOn: day("2026-10-16"), Term: 3, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "13.55"},
		// Ozon, накопительный: с 3-го периода 12,7 % при 1,4 млн ₽ в банке.
		{"Ozon, накопительный", "ozon-nakopitelny", Holding{Variant: "minimum", OpenedOn: day("2026-01-10")}, "2026-10-20", 1_500_000_00, map[string]int{"balance": 0}, "12.7"},
		// ОТП «Горизонт»: 181 день, от 1,5 млн — 13,5 %; накопительный 11,5 % до 2 млн, сверх — ничего.
		{"ОТП, Горизонт", "otp-gorizont", Holding{Variant: "end", OpenedOn: day("2026-10-16"), Term: 181, Amount: 2_000_000_00}, "2026-10-20", 2_000_000_00, nil, "13.5"},
		{"ОТП, накопительный", "otp-nakopitelny", Holding{Variant: "daily", OpenedOn: day("2026-01-10")}, "2026-10-20", 4_000_000_00, nil, "5.75"},
		// ТКБ «Ключевой доход», 105 дней: КС, КС − 0,15, КС − 0,3 по периодам — 14 / 13,85 / 13,7.
		{"ТКБ, первый период", "tkb-klyuchevoy-dohod", Holding{Variant: "d105", OpenedOn: day("2026-10-16"), Term: 105, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "14"},
		{"ТКБ, третий период", "tkb-klyuchevoy-dohod", Holding{Variant: "d105", OpenedOn: day("2026-10-16"), Term: 105, Amount: 100_000_00}, "2027-01-10", 100_000_00, nil, "13.7"},
		// Кубань Кредит: «Управляемый» — ½ КС; «Ключевой» 367–545 дней от 1 млн — КС − 1,2.
		{"Кубань, Управляемый", "kubankredit-upravlyaemy", Holding{Variant: "capitalize", OpenedOn: day("2026-10-16"), Term: 1100, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "7"},
		{"Кубань, Ключевой", "kubankredit-klyuchevoy", Holding{Variant: "monthly", OpenedOn: day("2026-10-16"), Term: 400, Amount: 1_500_000_00}, "2026-10-20", 1_500_000_00, nil, "12.8"},
		// Новиком «Капитал Сезонный», 121 день — 14,2 %.
		{"Новиком", "novikom-kapital-sezonny", Holding{Variant: "end", OpenedOn: day("2026-10-16"), Term: 121, Amount: 200_000_00}, "2026-10-20", 200_000_00, nil, "14.2"},
		// Шаг 4.5 (решение владельца к Р-55): программы для категорий, премиальные, связки с ПДС, в валюте.
		// СберВклад Премиум, уровень 6: «до 13,70 %» на 3–7 месяцев.
		{"СберВклад, уровень 6", "sber-sbervklad", Holding{Variant: "end", OpenedOn: day("2026-10-16"), Term: 6, Amount: 1_200_000_00}, "2026-10-20", 1_200_000_00, map[string]int{"prime": 3}, "13.7"},
		// Накопительный Премиум, уровни 4–6: «до 13 %» — инвестиции от 5 млн и покупки от 100 000 ₽.
		{"Сбер, накопительный Премиум", "sber-nakopitelny-premium", Holding{Variant: "levels4-6", OpenedOn: day("2026-01-10")}, "2026-10-20", 20_000_000_00, map[string]int{"invest": 2, "spend": 2}, "13"},
		// Молодёжный: максимум 13,3 % (14–17) и 13,5 % (18–24).
		{"Сбер, молодёжный 14–17", "sber-molodezhny", Holding{Variant: "y14-end", OpenedOn: day("2026-10-16"), Term: 6, Amount: 100_000_00}, "2026-10-20", 100_000_00, map[string]int{"prime": 0, "salary": 0, "partners": 0}, "13.3"},
		{"Сбер, молодёжный 18–24", "sber-molodezhny", Holding{Variant: "y18-end", OpenedOn: day("2026-10-16"), Term: 6, Amount: 100_000_00}, "2026-10-20", 100_000_00, map[string]int{"prime": 0, "salary": 0, "partners": 0}, "13.5"},
		{"Сбер, Забота о будущем", "sber-zabota", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 3, Amount: 100_000_00}, "2026-10-20", 100_000_00, map[string]int{"pds": 0}, "22"},
		{"Сбер, социальный", "sber-socialny", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 12, Amount: 50_000_00}, "2026-10-20", 50_000_00, nil, "22"},
		{"Сбер, в юанях", "sber-vklad-yuan", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 12, Amount: 200_000_00}, "2026-10-20", 200_000_00, nil, "1.25"},
		// ВТБ-Счёт «Привилегия»: зарплата и покупки от 100 000 ₽ — 13,75 %.
		{"ВТБ, Привилегия", "vtb-schet", Holding{Variant: "privilege-minimum", OpenedOn: day("2026-01-10")}, "2026-10-20", 500_000_00, map[string]int{"salary": 0, "spend": 1}, "13.75"},
		{"ВТБ, социальный", "vtb-socialny", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 365, Amount: 50_000_00}, "2026-10-20", 50_000_00, nil, "19"},
		{"ВТБ, Двойная выгода", "vtb-dvoynaya-vygoda", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 91, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "18"},
		{"Т-Банк, в юанях", "tbank-smartvklad", Holding{Variant: "fixed-cny", OpenedOn: day("2026-10-16"), Term: 12, Amount: 10_000_00}, "2026-10-20", 10_000_00, nil, "5"},
		{"ГПБ, Пенсионный доход", "gazprombank-pensionny-dohod", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 367, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "2.6"},
		{"ГПБ, в юанях", "gazprombank-rasshiryay", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 367, Amount: 10_000_00}, "2026-10-20", 10_000_00, nil, "2.47"},
		{"ГПБ, Премиум", "gazprombank-premium-ns", Holding{Variant: "minimum", OpenedOn: day("2026-01-10")}, "2026-10-20", 1_000_000_00, map[string]int{"premium": 0, "new_money": 1}, "13.5"},
		// Альфа-Счёт, Alfa Only: покупки от 100 000 ₽ — 12,5 %.
		{"Альфа, Alfa Only", "alfa-schet", Holding{Variant: "only-minimum", OpenedOn: day("2026-07-10")}, "2026-10-20", 1_000_000_00, map[string]int{"spend": 1}, "12.5"},
		// Уралсиб «Доход»: 367 дней, от 1 млн ₽, в конце срока — 14 %; «Доход Максимум» с договором ПДС — 25 %.
		{"Уралсиб, Доход", "uralsib-dohod", Holding{Variant: "end", OpenedOn: day("2026-10-16"), Term: 367, Amount: 1_000_000_00}, "2026-10-20", 1_000_000_00, nil, "14"},
		{"Уралсиб, Доход Максимум", "uralsib-dohod-maksimum", Holding{Variant: "end", OpenedOn: day("2026-10-16"), Term: 31, Amount: 200_000_00}, "2026-10-20", 200_000_00, map[string]int{"pds": 0}, "25"},
		{"Уралсиб, Комфорт Плюс", "uralsib-komfort-plus", Holding{Variant: "private", OpenedOn: day("2026-10-16"), Term: 1100, Amount: 60_000_000_00}, "2026-10-20", 60_000_000_00, nil, "13.7"},
		// ДОМ.РФ: «Вклад для пенсионеров» — «до 14,5 %» (Мой Дом 91 день + 0,7); Ультра — КС − 2.
		{"ДОМ.РФ, пенсионеру", "domrf-moy-dom", Holding{Variant: "none-end", OpenedOn: day("2026-10-16"), Term: 91, Amount: 100_000_00}, "2026-10-20", 100_000_00, map[string]int{"pensioner": 0}, "14.5"},
		{"ДОМ.РФ, Ультра", "domrf-ultra", Holding{Variant: "ultra", OpenedOn: day("2026-01-10")}, "2026-10-20", 3_000_000_00, nil, "12"},
		{"ДОМ.РФ, в юанях", "domrf-moy-dom", Holding{Variant: "cny", OpenedOn: day("2026-10-16"), Term: 367, Amount: 10_000_00}, "2026-10-20", 10_000_00, nil, "3.5"},
		{"РСХБ, Пенсионный Плюс", "rshb-pensionny-plus", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 395, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "6"},
		{"БСПБ, Пенсионный", "bspb-pensionny", Holding{Variant: "end", OpenedOn: day("2026-10-16"), Term: 3, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "15.2"},
		{"Россия, Стабильное будущее", "rossiya-stabilnoe-budushchee", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 91, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "19.5"},
		{"ПСБ, Народный пенсионеру", "psb-narodny", Holding{Variant: "pensioner", OpenedOn: day("2026-10-16"), Term: 91, Amount: 50_000_00}, "2026-10-20", 50_000_00, nil, "27"},
		{"ПСБ, Ставка на будущее", "psb-stavka-na-budushchee", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 32, Amount: 100_000_00}, "2026-10-20", 100_000_00, nil, "28"},
		// Кубань Кредит «Пенсионный специальный»: ½ КС на 100 000 ₽ и 0,4 КС сверх — 200 000 ₽ при ключевой 14 — 6,3.
		{"Кубань, пенсионный", "kubankredit-pensionny", Holding{Variant: "online", OpenedOn: day("2026-10-16"), Term: 1100, Amount: 200_000_00}, "2026-10-20", 200_000_00, nil, "6.3"},
		{"Кубань, детский", "kubankredit-detsky", Holding{Variant: "main", OpenedOn: day("2026-10-16"), Term: 365, Amount: 3_000_000_00}, "2026-10-20", 3_000_000_00, nil, "12.6"},
		{"Новиком, Капитал", "novikom-kapital", Holding{Variant: "rub", OpenedOn: day("2026-10-16"), Term: 91, Amount: 1_000_000_00}, "2026-10-20", 1_000_000_00, nil, "11"},
		{"Новиком, премиум онлайн", "novikom-kapital", Holding{Variant: "premium-online", OpenedOn: day("2026-10-16"), Term: 91, Amount: 60_000_000_00}, "2026-10-20", 60_000_000_00, nil, "13.8"},
		// Минимальные ставки Новикомбанка с 07.10: 181 день «премиум онлайн» — 13,1 % (страница — 13,3 и выше),
		// «Premium Капитал» в юанях на 367 дней — 2,8 % от 1 млн ¥.
		{"Новиком, премиум онлайн 181", "novikom-kapital", Holding{Variant: "premium-online", OpenedOn: day("2026-10-16"), Term: 181, Amount: 60_000_000_00}, "2026-10-20", 60_000_000_00, nil, "13.1"},
		{"Новиком, Premium в юанях", "novikom-kapital", Holding{Variant: "premium-cny", OpenedOn: day("2026-10-16"), Term: 367, Amount: 1_000_000_00}, "2026-10-20", 1_000_000_00, nil, "2.8"},
		// Сводные документы банков: ГПБ «Условия надбавок» — +5,5 за ПДС на 91 день; ВТБ «Тарифы по депозитам» —
		// «Привилегия» с зарплатой на 370 дней: 12 + 0,9; Россельхозбанк «Ультра» — 13,6 % на 4 месяца.
		{"ГПБ, Перспективные сбережения", "gazprombank-perspektivnye", Holding{Variant: "online", OpenedOn: day("2026-10-16"), Term: 91, Amount: 100_000_00}, "2026-10-20", 100_000_00, map[string]int{"pds": 0}, "15.5"},
		{"ВТБ-Вклад, Привилегия", "vtb-vklad", Holding{Variant: "privilege-online-end", OpenedOn: day("2026-10-16"), Term: 370, Amount: 2_000_000_00}, "2026-10-20", 2_000_000_00, map[string]int{"salary": 0}, "12.9"},
		{"РСХБ, Ультра", "rshb-dohodny-premium", Holding{Variant: "ultra-online-end", OpenedOn: day("2026-10-16"), Term: 4, Amount: 2_000_000_00}, "2026-10-20", 2_000_000_00, nil, "13.6"},
		{"ТКБ, Привилегия", "tkb-privilegiya", Holding{Variant: "main", OpenedOn: day("2026-10-17"), Term: 120, Amount: 3_000_000_00}, "2026-10-20", 3_000_000_00, nil, "13.76"},
	}
	for _, c := range cases {
		q, err := programme(t, c.programme).Quote(c.h, day(c.day), c.balance, key, marks(c.met))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !q.Percent.Equal(decimal.RequireFromString(c.want)) {
			t.Errorf("%s: %s, want %s (parts %+v)", c.name, q.Percent, c.want, q.Parts)
		}
	}
	// Альфа-Банк keeps its rates by the day of opening: an account of
	// February 2026 is of another table.
	_, err := programme(t, "alfa-schet").Quote(Holding{Variant: "minimum", OpenedOn: day("2026-02-10")}, day("2026-10-20"), 1, key, nil)
	if !errors.Is(err, ErrOpened) {
		t.Errorf("an account of February: %v", err)
	}
	// «Premium Капитал» opens from 3,5 million roubles: a smaller sum has no
	// rate of the line, not the premium one.
	_, err = programme(t, "novikom-kapital").Quote(Holding{Variant: "premium", OpenedOn: day("2026-10-16"), Term: 91, Amount: 1_000_000_00}, day("2026-10-20"), 1_000_000_00, key, nil)
	if !errors.Is(err, ErrNoCell) {
		t.Errorf("Premium Капитал under 3,5 million: %v", err)
	}
}

// The social deposit: by law its rate is the bank's highest on deposits of up
// to a year and moves with it — the deposit follows the editions day by day,
// unlike a fixed one that keeps the edition of its opening.
func TestAFloatingDepositFollowsTheEditions(t *testing.T) {
	cell := func(pct string) []Cell { return []Cell{{From: 365, To: 365, Rate: Rate{Percent: pct}}} }
	p := Programme{ID: "t", Kind: KindDeposit, Editions: []Edition{
		{From: "2026-01-01", Variants: []Variant{{ID: "social", TermUnit: TermDays, Floating: true, Grid: cell("16")}, {ID: "fixed", TermUnit: TermDays, Grid: cell("16")}}},
		{From: "2026-06-01", Variants: []Variant{{ID: "social", TermUnit: TermDays, Floating: true, Grid: cell("13")}, {ID: "fixed", TermUnit: TermDays, Grid: cell("13")}}},
	}}
	for variant, want := range map[string]string{"social": "13", "fixed": "16"} {
		q, err := p.Quote(Holding{Variant: variant, OpenedOn: day("2026-02-01"), Term: 365, Amount: 50_000_00}, day("2026-07-01"), 50_000_00, nil, nil)
		pct(t, q, err, want)
	}
	if money, _ := p.VariantMoney("social"); money != "RUB" {
		t.Errorf("a variant without a currency is in %s, want roubles", money)
	}
}

// Whom the variants are open to (Р-57 Б, the comparison's filter): a few
// written out, so that a slip in the catalog shows.
func TestWhomTheVariantsAreOpenTo(t *testing.T) {
	for _, c := range []struct {
		programme, variant string
		want               []Audience
	}{
		{"sber-sbervklad", "end", []Audience{AudienceAll}},
		{"sber-socialny", "main", []Audience{AudienceSocial}},
		{"psb-narodny", "pensioner", []Audience{AudienceNew, AudiencePensioner}},
		{"vtb-vklad", "privilege-online-end", []Audience{AudiencePremium}},
		{"vtb-vklad", "online-end", []Audience{AudienceAll}},
		{"novikom-kapital", "salary-online", []Audience{AudienceSalary}},
		{"uralsib-dohod-maksimum", "end", []Audience{AudienceBundle}},
		{"gazprombank-premium-ns", "minimum", []Audience{AudienceClosed}},
		{"sber-molodezhny", "y18-end", []Audience{AudienceYouth}},
	} {
		p := programme(t, c.programme)
		v, ok := p.Editions[len(p.Editions)-1].Variant(c.variant)
		if !ok || !slices.Equal(v.Audience, c.want) {
			t.Errorf("%s %s: %v, want %v", c.programme, c.variant, v.Audience, c.want)
		}
	}
}

// The Bank of Russia's forecast of 24 July 2026: the rest of 2026 13,7–14,0 —
// 13,85; 2027 10,5–12,5 — 11,5; 2029's 8 for 2031; nothing for 2025.
func TestTheKeyRateForecast(t *testing.T) {
	f, err := Forecast()
	if err != nil {
		t.Fatal(err)
	}
	for on, want := range map[string]string{"2026-10-19": "13.85", "2026-03-01": "14.55", "2027-06-01": "11.5", "2031-01-01": "8"} {
		got, ok := f.On(day(on))
		if !ok || !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("%s: %s %v, want %s", on, got, ok, want)
		}
	}
	if _, ok := f.On(day("2025-12-31")); ok {
		t.Error("2025: a forecast")
	}
}

// Decision Р-59 Б: ПСБ's savings accounts by the bank's archive of their
// terms from 30.12.2025 — a year's estimate needs every day of it.
// «Акцент на процент» with purchases of 250 000 ₽ a month: 10 + 7 in
// January, 10 + 6 from 16.01, 8 + 3,5 for 10 000 ₽ in April, the base 7 in
// July, 5 from 14.08. «Хранитель» on the minimum balance: 0,01 % under
// 10 000 ₽ till June, 14 above it in January, 12,5 from 19.02, 10,5 for any
// sum from 03.06. «Про запас» to a new client opened in May: 13,5 for two
// months, then 5.
func TestPSBSavingsBeforeTheCatalog(t *testing.T) {
	akcent := programme(t, "psb-akcent")
	h := Holding{Variant: "daily", OpenedOn: day("2025-06-01")}
	for _, c := range []struct {
		on    string
		level map[string]int
		want  string
	}{
		{"2026-01-10", map[string]int{"spend": 4}, "17"},
		{"2026-02-10", map[string]int{"spend": 4}, "16"},
		{"2026-04-10", map[string]int{"spend": 0}, "11.5"},
		{"2026-07-10", nil, "7"},
		{"2026-09-10", nil, "5"},
	} {
		q, err := akcent.Quote(h, day(c.on), 500_000_00, nil, marks(c.level))
		pct(t, q, err, c.want)
	}
	keeper := programme(t, "psb-hranitel")
	minimum := Holding{Variant: "minimum", OpenedOn: day("2025-01-01")}
	for _, c := range []struct {
		on      string
		balance int64
		want    string
	}{
		{"2026-01-10", 5_000_00, "0.01"},
		{"2026-01-10", 100_000_00, "14"},
		{"2026-02-20", 100_000_00, "12.5"},
		{"2026-06-10", 5_000_00, "10.5"},
	} {
		q, err := keeper.Quote(minimum, day(c.on), c.balance, nil, nil)
		pct(t, q, err, c.want)
	}
	zapas := programme(t, "psb-pro-zapas")
	fresh := Holding{Variant: "minimum", OpenedOn: day("2026-05-10")}
	q, err := zapas.Quote(fresh, day("2026-05-20"), 300_000_00, nil, marks(map[string]int{"new_client": 0}))
	pct(t, q, err, "13.5")
	q, err = zapas.Quote(fresh, day("2026-07-20"), 300_000_00, nil, marks(map[string]int{"new_client": 0}))
	pct(t, q, err, "5")
}

// Decision Р-59 Б: ДОМ.РФ's savings account by the bank's tables from
// 26.12.2025 — the first two months' rate 15,4 in January, 14,9 from 20.02,
// 13,6 from 29.04, 13,4 from 30.05; 4 % after them all year. «Ультра» is
// the key rate less 2 from 26.12.2025 too.
func TestDomRFSavingsBeforeTheCatalog(t *testing.T) {
	p := programme(t, "domrf-nakopitelny")
	for _, c := range []struct{ opened, on, want string }{
		{"2026-01-05", "2026-01-10", "15.4"},
		{"2026-02-25", "2026-03-10", "14.9"},
		{"2026-05-05", "2026-05-20", "13.6"},
		{"2026-06-01", "2026-07-15", "13.4"},
		{"2025-11-01", "2026-02-10", "4"},
	} {
		q, err := p.Quote(Holding{Variant: "daily", OpenedOn: day(c.opened)}, day(c.on), 100_000_00, nil, nil)
		pct(t, q, err, c.want)
	}
	q, err := programme(t, "domrf-ultra").Quote(Holding{Variant: "ultra", OpenedOn: day("2025-06-01")}, day("2026-01-10"), 1_000_000_00, keys("2025-10-27", "16.5", "2025-12-22", "16"), nil)
	pct(t, q, err, "14")
}

// Decision Р-59 Б: Сбер's savings account by the bank's archive of rates —
// 9 % in the first days of January, 8 % from 05.01 (and 13,5 the first two
// months of a new client's account), 7,5 % from 20.02; from 17.03 to 29.04
// the archive shows none, so those days have no rate by the catalog.
func TestSberSavingsBeforeTheCatalog(t *testing.T) {
	p := programme(t, "sber-nakopitelny")
	old := Holding{Variant: "daily", OpenedOn: day("2025-05-01")}
	for _, c := range []struct{ on, want string }{{"2026-01-03", "9"}, {"2026-02-01", "8"}, {"2026-03-01", "7.5"}} {
		q, err := p.Quote(old, day(c.on), 100_000_00, nil, nil)
		pct(t, q, err, c.want)
	}
	fresh := Holding{Variant: "daily", OpenedOn: day("2026-01-10")}
	q, err := p.Quote(fresh, day("2026-01-20"), 100_000_00, nil, marks(map[string]int{"new_client": 0}))
	pct(t, q, err, "13.5")
	if _, err := p.Quote(old, day("2026-04-01"), 100_000_00, nil, nil); err == nil {
		t.Error("a rate between the archive and the catalog")
	}
}

// An edition's last day is within it and before the next edition.
func TestAnEditionsLastDayIsChecked(t *testing.T) {
	file := func(until string) string {
		return `
id: x
bank: vtb
name: X
kind: savings
editions:
  - from: 2026-01-01
    until: ` + until + `
    checked_on: 2026-01-02
    sources: [https://example.ru]
    variants:
      - { id: a, audience: [all], accrual: daily, payouts: [capitalize], ops: topup_withdraw, grid: [{ percent: "10" }] }
  - from: 2026-03-01
    checked_on: 2026-03-02
    sources: [https://example.ru]
    variants:
      - { id: a, audience: [all], accrual: daily, payouts: [capitalize], ops: topup_withdraw, grid: [{ percent: "9" }] }
`
	}
	if _, err := readCatalog(fstest.MapFS{"catalog/x.yaml": {Data: []byte(file("2026-02-15"))}}); err != nil {
		t.Fatalf("a good last day: %v", err)
	}
	for _, bad := range []string{"2025-12-31", "2026-03-01", "15.02.2026"} {
		if _, err := readCatalog(fstest.MapFS{"catalog/x.yaml": {Data: []byte(file(bad))}}); err == nil {
			t.Errorf("until %s: read", bad)
		}
	}
}
