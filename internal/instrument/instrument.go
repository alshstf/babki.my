// Package instrument owns the global instrument catalog. The catalog is
// instance-wide (no space scoping): reference data is shared, and in later
// plans it is auto-populated from market data providers.
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

// Update describes a partial update; nil = unchanged, double pointers
// follow the tri-state pattern established in the account module.
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

// NormalizeISIN brings an ISIN a person typed to the one spelling the catalog
// and the corporate-actions registry match on: trimmed and upper-cased. Empty
// stays empty — an instrument need not have one.
//
// Matching is by string equality everywhere, so "ru000a101x68" used to be a
// different paper from "RU000A101X68": a second catalog row, and an event that
// never found its holders and said nothing about it (#202).
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
