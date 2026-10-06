// Package instrument owns the instance-wide instrument catalog.
package instrument

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Type string

const (
	TypeShare    Type = "share"
	TypeBond     Type = "bond"
	TypeETF      Type = "etf"
	TypeCurrency Type = "currency"
	TypeCrypto   Type = "crypto"
	TypeMetal    Type = "metal"
	TypeCustom   Type = "custom"
)

var validTypes = map[Type]bool{
	TypeShare: true, TypeBond: true, TypeETF: true, TypeCurrency: true,
	TypeCrypto: true, TypeMetal: true, TypeCustom: true,
}

func (t Type) Valid() bool { return validTypes[t] }

type Instrument struct {
	ID             uuid.UUID
	Type           Type
	Name           string
	Ticker         string
	ISIN           string
	FIGI           string
	Currency       string
	FaceValueMinor *int64  // bonds: face value in minor units
	FaceCurrency   *string // bonds: face value currency
	Frozen         bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Update is a partial update; nil fields are unchanged, double pointers clear.
type Update struct {
	Name           *string
	Ticker         *string
	ISIN           *string
	FIGI           *string
	Frozen         *bool
	FaceValueMinor **int64
	FaceCurrency   **string
}

// isinRe is the shape of an ISIN (ISO 6166): two letters of a country, nine
// alphanumerics, one check digit. The check digit itself is not verified.
var isinRe = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{9}[0-9]$`)

// ErrBadISIN reports an ISIN that is not one even after NormalizeISIN.
var ErrBadISIN = errors.New("isin must be two letters, nine letters or digits and a digit, e.g. US0231351067")

// ForeignISIN reports a paper whose issuer is not Russian, by its ISIN's
// country: such a paper's dividend tax is taken abroad, and its home exchange
// is not Moscow. A paper with no ISIN is not known to be foreign.
func ForeignISIN(isin string) bool {
	isin = strings.ToUpper(strings.TrimSpace(isin))
	return isin != "" && !strings.HasPrefix(isin, "RU")
}

// NormalizeISIN trims and upper-cases an ISIN, the spelling the catalog and the
// registry match on (#202). Empty stays empty.
func NormalizeISIN(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if !isinRe.MatchString(s) {
		return "", ErrBadISIN
	}
	return s, nil
}
