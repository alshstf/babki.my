package family

import (
	"testing"
	"time"
)

// Russia's treaties as the Ministry of Finance listed them on 14.07.2026
// (decision Р-50): Kazakhstan's applies; the United States' is suspended in
// part since 08.08.2023 — its article on crediting stands; Germany's in part,
// then whole from 01.01.2027; Lithuania's ended on 01.01.2026; the United
// Arab Emirates' applies to people from 01.01.2026; with the Cayman Islands
// there is none.
func TestRussiasTreaties(t *testing.T) {
	ru, ok, err := TreatiesOf("RU")
	if err != nil || !ok {
		t.Fatalf("RU: %v %v", ok, err)
	}
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	for _, c := range []struct {
		country, on string
		want        TreatyState
	}{
		{"KZ", "2026-05-15", TreatyApplies},
		{"US", "2023-08-07", TreatyApplies},
		{"US", "2026-05-15", TreatyPartly},
		{"DE", "2026-12-31", TreatyPartly},
		{"DE", "2027-01-01", TreatySuspended},
		{"LT", "2025-12-31", TreatyPartly},
		{"LT", "2026-01-01", TreatyEnded},
		{"GB", "2025-04-01", TreatySuspended},
		{"AE", "2025-12-31", TreatyNone},
		{"AE", "2026-01-01", TreatyApplies},
		{"KY", "2026-05-15", TreatyNone},
	} {
		if _, got := ru.Of(c.country, day(c.on)); got != c.want {
			t.Errorf("%s on %s = %s, want %s", c.country, c.on, got, c.want)
		}
	}
	if us, _ := ru.Of("US", day("2026-05-15")); us.Name != "США" || us.SuspendedArticles == "" {
		t.Errorf("US = %+v", us)
	}
	if !TreatyPartly.Credits() || TreatySuspended.Credits() || TreatyNone.Credits() {
		t.Error("only a treaty applying, whole or in part, credits")
	}
	if _, ok, _ := TreatiesOf("DE"); ok {
		t.Error("the application keeps no German treaties")
	}
	if len(ru.All()) < 80 || ru.SourceAsOf != "2026-07-14" {
		t.Errorf("%d treaties as of %s", len(ru.All()), ru.SourceAsOf)
	}
}
