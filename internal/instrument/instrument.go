// Package instrument owns the global instrument catalog. The catalog is
// instance-wide (no space scoping): reference data is shared, and in later
// plans it is auto-populated from market data providers.
package instrument

import (
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
