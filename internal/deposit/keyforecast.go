package deposit

import (
	_ "embed"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"gopkg.in/yaml.v3"
)

//go:embed keyforecast.yaml
var keyForecastFile []byte

// KeyForecast is the Bank of Russia's forecast of the average key rate by
// year, as published on Published in Source.
type KeyForecast struct {
	Published string    `yaml:"published"`
	Source    string    `yaml:"source"`
	Years     []KeyYear `yaml:"years"`
}

// KeyYear is a year's range of the average key rate; Rest the range of the
// part of the year left from Since, when the forecast gives it.
type KeyYear struct {
	Year int      `yaml:"year"`
	From string   `yaml:"from"`
	To   string   `yaml:"to"`
	Rest *KeyRest `yaml:"rest"`
}

// KeyRest is the range of the rest of a year.
type KeyRest struct {
	Since string `yaml:"since"`
	From  string `yaml:"from"`
	To    string `yaml:"to"`
}

// Forecast is the forecast built into the program.
func Forecast() (KeyForecast, error) {
	var f KeyForecast
	if err := yaml.Unmarshal(keyForecastFile, &f); err != nil {
		return KeyForecast{}, fmt.Errorf("key forecast: %w", err)
	}
	if _, err := time.Parse(time.DateOnly, f.Published); err != nil || f.Source == "" || len(f.Years) == 0 {
		return KeyForecast{}, fmt.Errorf("key forecast: a publication day, a source and years")
	}
	for i, y := range f.Years {
		if _, err := mid(y.From, y.To); err != nil || i > 0 && y.Year != f.Years[i-1].Year+1 {
			return KeyForecast{}, fmt.Errorf("key forecast: year %d: a range, the years in a row", y.Year)
		}
		if r := y.Rest; r != nil {
			if _, err := mid(r.From, r.To); err != nil {
				return KeyForecast{}, fmt.Errorf("key forecast: year %d: the rest's range", y.Year)
			}
			if _, err := time.Parse(time.DateOnly, r.Since); err != nil {
				return KeyForecast{}, fmt.Errorf("key forecast: year %d: the rest's day", y.Year)
			}
		}
	}
	return f, nil
}

// On is the key rate the forecast gives for day: the middle of the range of
// its year — of the rest of the year from its day on — the last year's after
// the forecast ends; ok is false before it begins.
func (f KeyForecast) On(day time.Time) (decimal.Decimal, bool) {
	for _, y := range f.Years {
		if y.Year != day.Year() {
			continue
		}
		if r := y.Rest; r != nil && day.Format(time.DateOnly) >= r.Since {
			m, _ := mid(r.From, r.To)
			return m, true
		}
		m, _ := mid(y.From, y.To)
		return m, true
	}
	if n := len(f.Years); n > 0 && day.Year() > f.Years[n-1].Year {
		last := f.Years[n-1]
		m, _ := mid(last.From, last.To)
		return m, true
	}
	return decimal.Zero, false
}

func mid(from, to string) (decimal.Decimal, error) {
	a, err := decimal.NewFromString(from)
	if err != nil {
		return decimal.Zero, err
	}
	b, err := decimal.NewFromString(to)
	if err != nil || b.LessThan(a) {
		return decimal.Zero, fmt.Errorf("a range %s–%s", from, to)
	}
	return a.Add(b).Div(decimal.NewFromInt(2)), nil
}
