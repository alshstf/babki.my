package taxyear

import (
	"errors"
	"fmt"

	"golang.org/x/text/language"
)

// countryCode is a country's digital code of ОКСМ, the return's lines 010 and
// 011 — ISO 3166-1 numeric, which ОКСМ follows; empty for no country, as an
// ISIN's XS (a paper cleared internationally, its issuer's country not in it).
// countryCodeOf is countryCode, refusing what is no country.
func countryCodeOf(alpha2 string) (string, error) {
	if c := countryCode(alpha2); c != "" {
		return c, nil
	}
	return "", errors.New("taxyear: no country")
}

func countryCode(alpha2 string) string {
	r, err := language.ParseRegion(alpha2)
	if err != nil || len(alpha2) != 2 {
		return ""
	}
	n := r.M49()
	// 900 and above are private use: XS, XA…; 0 is no region.
	if n == 0 || n >= 900 {
		return ""
	}
	return fmt.Sprintf("%03d", n)
}

// currencyCodes are ОКВ's digital codes, the return's line 030 — ISO 4217
// numeric, which ОКВ follows — of the currencies a broker abroad pays in.
var currencyCodes = map[string]int{
	"AED": 784, "AMD": 51, "AUD": 36, "AZN": 944, "BRL": 986, "BYN": 933, "CAD": 124, "CHF": 756, "CNY": 156,
	"CZK": 203, "DKK": 208, "EUR": 978, "GBP": 826, "GEL": 981, "HKD": 344, "ILS": 376, "INR": 356, "JPY": 392,
	"KGS": 417, "KRW": 410, "KZT": 398, "MXN": 484, "NOK": 578, "NZD": 554, "PLN": 985, "RUB": 643, "SEK": 752,
	"SGD": 702, "THB": 764, "TJS": 972, "TRY": 949, "UAH": 980, "USD": 840, "UZS": 860, "ZAR": 710,
}

// currencyCode is a currency's digital code of ОКВ; empty for one not in
// the table.
func currencyCode(code string) string {
	n, ok := currencyCodes[code]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%03d", n)
}
