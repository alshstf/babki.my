package operation

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
)

// tradable is every kind of paper that is bought and sold as one. Currency is
// not: money changes hands by an exchange of money, not as a holding.
var tradable = []instrument.Type{
	instrument.TypeShare, instrument.TypeETF, instrument.TypeBond,
	instrument.TypeCrypto, instrument.TypeMetal, instrument.TypeCustom,
}

// kindsFor is which kinds of paper a hand entry of each type may name (decision
// Р-17). A type not listed may name any: a fee or a tax is charged on anything.
// A hand-made paper (custom) takes every income a security can pay.
var kindsFor = map[Type][]instrument.Type{
	TypeBuy:          tradable,
	TypeSell:         tradable,
	TypeRedemption:   {instrument.TypeBond, instrument.TypeETF, instrument.TypeCustom},
	TypeDividend:     {instrument.TypeShare, instrument.TypeETF, instrument.TypeCustom},
	TypeCoupon:       {instrument.TypeBond, instrument.TypeCustom},
	TypeAmortization: {instrument.TypeBond, instrument.TypeCustom},
}

// KindsFor is kindsFor for the web forms' generated copy (cmd/webconst).
func KindsFor() map[Type][]instrument.Type {
	out := make(map[Type][]instrument.Type, len(kindsFor))
	for t, kinds := range kindsFor {
		out[t] = slices.Clone(kinds)
	}
	return out
}

// fitsKind refuses a type of entry that cannot happen to a paper of kind.
func fitsKind(t Type, kind instrument.Type) error {
	kinds, ruled := kindsFor[t]
	if !ruled || slices.Contains(kinds, kind) {
		return nil
	}
	return fmt.Errorf("%w: %s cannot be recorded against a paper of kind %s", family.ErrValidation, t, kind)
}

// checkKind is fitsKind for a hand entry's paper, read from the catalog. A
// paper missing from it is the foreign key's to refuse.
func (s *Store) checkKind(ctx context.Context, o Operation) error {
	if o.InstrumentID == nil {
		return nil
	}
	kind, err := s.instrumentKind(ctx, *o.InstrumentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fitsKind(o.Type, kind)
}

func (s *Store) instrumentKind(ctx context.Context, id uuid.UUID) (instrument.Type, error) {
	var kind instrument.Type
	err := s.db.QueryRow(ctx, `SELECT type FROM instruments WHERE id = $1`, id).Scan(&kind)
	return kind, err
}
