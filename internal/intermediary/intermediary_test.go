package intermediary

import "testing"

// The catalog holds: ids and names unique within a kind, every bank with its
// number, every broker's bank in it.
func TestTheCatalogHolds(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.All) < 40 || c.CheckedOn == "" {
		t.Fatalf("catalog = %d entries, checked %q", len(c.All), c.CheckedOn)
	}
	if b, ok := c.ByID("tbank"); !ok || b.Reg != 2673 || !b.Insured() || b.Foreign() {
		t.Errorf("tbank = %+v", b)
	}
	if b, ok := c.ByID("freedom"); !ok || !b.Foreign() || b.Insured() {
		t.Errorf("freedom = %+v", b)
	}
}

// An account's «bank or broker» found by any of its names, whatever the
// case, quotes, «ё» and legal form; a bank and its brokerage told apart.
func TestAnAccountsIntermediaryIsMatched(t *testing.T) {
	c, _ := Load()
	for _, tc := range []struct {
		name string
		kind Kind
		want string
	}{
		{"Тинькофф", Bank, "tbank"},
		{"АО «ТБанк»", Bank, "tbank"},
		{"Т-Банк", Bank, "tbank"},
		{"тинькофф инвестиции", Broker, "tinvest"},
		{"ПАО Сбербанк", Bank, "sber"},
		{"Freedom Finance", Broker, "freedom"},
		{"Банк «Санкт-Петербург»", Bank, "bspb"},
		{"Альфа-Банк", Bank, "alfa"},
		{"Т-Банк", Broker, "tinvest"},
		{"Сбер", Broker, "sberinvest"},
	} {
		got, ok := c.Match(tc.name, tc.kind)
		if !ok || got.ID != tc.want {
			t.Errorf("%q (%s) = %q, %v; want %q", tc.name, tc.kind, got.ID, ok, tc.want)
		}
	}
	if got, ok := c.Match("Freedom Finance", Bank); ok {
		t.Errorf("a broker matched as a bank: %+v", got)
	}
	if _, ok := c.Match("Мой кошелёк", Bank); ok {
		t.Error("an unknown name matched")
	}
}
