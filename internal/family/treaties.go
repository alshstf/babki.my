package family

import (
	"fmt"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Treaty is one of a residency's agreements on avoiding double taxation
// (decision Р-50): when it applies, when some of its articles were suspended
// — the incomes and their rates, not the article that eliminates double
// taxation — when the partner suspended it whole and when it ended. A tax
// paid abroad is credited as the treaty provides; with no treaty applying,
// it is not.
type Treaty struct {
	Country             string     `yaml:"country"`
	Name                string     `yaml:"name"`
	AppliesFrom         time.Time  `yaml:"applies_from"`
	PartlySuspendedFrom *time.Time `yaml:"partly_suspended_from"`
	SuspendedArticles   string     `yaml:"suspended_articles"`
	SuspendedFrom       *time.Time `yaml:"suspended_from"`
	EndedFrom           *time.Time `yaml:"ended_from"`
	Note                string     `yaml:"note"`
}

// TreatyState is a treaty as it stands on a day.
type TreatyState string

const (
	// TreatyApplies: in force whole; a tax paid there is credited.
	TreatyApplies TreatyState = "applies"
	// TreatyPartly: some articles suspended, the one on crediting not; a tax
	// paid there is still credited, the treaty's lower rates are not given.
	TreatyPartly TreatyState = "partly"
	// TreatySuspended: the partner suspended it whole; nothing is credited.
	TreatySuspended TreatyState = "suspended"
	// TreatyEnded: terminated; nothing is credited.
	TreatyEnded TreatyState = "ended"
	// TreatyNone: no treaty with the country, or not yet applying.
	TreatyNone TreatyState = "none"
)

// Credits says a tax paid abroad is credited under the treaty in this state.
func (s TreatyState) Credits() bool { return s == TreatyApplies || s == TreatyPartly }

// On is the treaty's state on day.
func (t Treaty) On(day time.Time) TreatyState {
	switch {
	case t.EndedFrom != nil && !day.Before(*t.EndedFrom):
		return TreatyEnded
	case t.SuspendedFrom != nil && !day.Before(*t.SuspendedFrom):
		return TreatySuspended
	case day.Before(t.AppliesFrom):
		return TreatyNone
	case t.PartlySuspendedFrom != nil && !day.Before(*t.PartlySuspendedFrom):
		return TreatyPartly
	}
	return TreatyApplies
}

// Treaties are a residency's agreements by the partner's country, as the
// ministry's list stood on SourceAsOf, checked on CheckedOn.
type Treaties struct {
	CheckedOn  string
	SourceAsOf string
	byCountry  map[string]Treaty
}

// Of is the treaty with country and its state on day; TreatyNone without one.
func (t Treaties) Of(country string, day time.Time) (Treaty, TreatyState) {
	tr, ok := t.byCountry[country]
	if !ok {
		return Treaty{}, TreatyNone
	}
	return tr, tr.On(day)
}

// All are the treaties, for the monthly check against the ministry's list.
func (t Treaties) All() []Treaty {
	out := make([]Treaty, 0, len(t.byCountry))
	for _, tr := range t.byCountry {
		out = append(out, tr)
	}
	return out
}

var (
	treatiesOnce sync.Once
	treaties     map[string]Treaties
	treatiesErr  error
)

// TreatiesOf are the residency's treaties; false when the application keeps
// none for it.
func TreatiesOf(residency string) (Treaties, bool, error) {
	treatiesOnce.Do(func() {
		treaties = map[string]Treaties{}
		for country, raw := range treatyBooks {
			t, err := parseTreaties(raw)
			if err != nil {
				treatiesErr = fmt.Errorf("family: %s treaties: %w", country, err)
				return
			}
			treaties[country] = t
		}
	})
	if treatiesErr != nil {
		return Treaties{}, false, treatiesErr
	}
	t, ok := treaties[residency]
	return t, ok, nil
}

func parseTreaties(raw []byte) (Treaties, error) {
	var file struct {
		CheckedOn  string   `yaml:"checked_on"`
		SourceAsOf string   `yaml:"source_as_of"`
		Treaties   []Treaty `yaml:"treaties"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return Treaties{}, err
	}
	out := Treaties{CheckedOn: file.CheckedOn, SourceAsOf: file.SourceAsOf, byCountry: map[string]Treaty{}}
	for _, t := range file.Treaties {
		switch {
		case !taxResidencyRe.MatchString(t.Country) || t.Name == "":
			return Treaties{}, fmt.Errorf("an entry without a country code or a name: %+v", t)
		case t.AppliesFrom.IsZero():
			return Treaties{}, fmt.Errorf("%s: no day it applies from", t.Country)
		case t.PartlySuspendedFrom != nil && t.SuspendedArticles == "":
			return Treaties{}, fmt.Errorf("%s: suspended in part, but which articles", t.Country)
		}
		if _, dup := out.byCountry[t.Country]; dup {
			return Treaties{}, fmt.Errorf("%s twice", t.Country)
		}
		out.byCountry[t.Country] = t
	}
	return out, nil
}
