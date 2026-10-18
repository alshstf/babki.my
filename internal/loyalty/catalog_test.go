package loyalty

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func names(cs []Category) string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return strings.Join(out, ", ")
}

// Decision Р-58: every programme of the catalog reads and holds together;
// a purchase is told into its categories by its code — a code of a range
// too, a code with its leading zero; ВТБ's code of ЖКХ is excluded from a
// bonus for everything, yet its category «Услуги ЖКХ» takes it.
func TestTheLoyaltyCatalog(t *testing.T) {
	all, err := Catalog()
	if err != nil || len(all) < 2 {
		t.Fatalf("catalog = %d programmes, %v", len(all), err)
	}
	on := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	vtb, ok := ByID("vtb-multibonus")
	if !ok {
		t.Fatal("no Мультибонус")
	}
	e, ok := vtb.EditionOn(on)
	if !ok || e.Period != PeriodMonth || len(e.Variants) != 6 {
		t.Fatalf("edition = %+v", e)
	}
	for mcc, want := range map[string]string{
		"5411": "Супермаркеты",
		"3100": "Авиабилеты",
		"3600": "Отели",
		"0742": "Зоотовары",
		"4900": "Услуги ЖКХ",
		"6011": "",
		"742":  "",
	} {
		if got := names(e.CategoriesOf(mcc)); got != want {
			t.Errorf("ВТБ %s = %q, want %q", mcc, got, want)
		}
	}
	if !e.Excludes("4900") || !e.Excludes("6011") || e.Excludes("5411") {
		t.Error("ВТБ's exclusions")
	}
	tb, _ := ByID("tbank-black")
	te, _ := tb.EditionOn(on)
	if got := names(te.CategoriesOf("5977")); got != "Косметика, Красота" {
		t.Errorf("Т-Банк 5977 = %q: a code in two categories is in both", got)
	}
	if got := names(te.CategoriesOf("3304")); got != "" {
		t.Errorf("Т-Банк 3304 = %q: past the range of flights", got)
	}
	if v := te.Variants[0]; v.Pick != 4 || v.Cap != 300000 {
		t.Errorf("Black = %+v", v)
	}
	if _, ok := tb.EditionOn(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Error("an edition before the first")
	}
}

// A programme that cannot be told by is refused: a code of three digits, a
// range backwards, a choice of more than is offered, a programme without a
// choice that picks, an unknown period.
func TestABrokenLoyaltyProgrammeIsRefused(t *testing.T) {
	good := `id: x
bank: tbank
name: X
editions:
  - from: 2026-10-01
    checked_on: 2026-10-19
    sources: [https://example.ru]
    period: month
    variants: [{ id: a, name: A, pick: 2, of: 5 }]
    categories: [{ id: c, name: C, mcc: ["5411", "3000-3301"] }]
`
	if _, err := readCatalog(fstest.MapFS{"catalog/x.yaml": {Data: []byte(good)}}); err != nil {
		t.Fatalf("the good one = %v", err)
	}
	for name, broken := range map[string]string{
		"a three-digit code":       strings.Replace(good, `"5411"`, `"541"`, 1),
		"a range backwards":        strings.Replace(good, `"3000-3301"`, `"3301-3000"`, 1),
		"more picked than offered": strings.Replace(good, "pick: 2, of: 5", "pick: 6, of: 5", 1),
		"a pick without a choice":  strings.Replace(good, "period: month", "period: none", 1),
		"an unknown period":        strings.Replace(good, "period: month", "period: week", 1),
		"an unknown bank":          strings.Replace(good, "bank: tbank", "bank: nobank", 1),
	} {
		if _, err := readCatalog(fstest.MapFS{"catalog/x.yaml": {Data: []byte(broken)}}); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}
