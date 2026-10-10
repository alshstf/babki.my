// Package receipt keeps the family's cash receipts (household stage 3, the
// receipts plan): the shop's document of a purchase, named for good by its
// fiscal drive (FN) and the document's number on it (FD). A receipt completes
// a row of the journal — the bank's row of a card purchase, or the one
// written by hand for cash — rather than standing for a second purchase; the
// seller and the items it brings (decisions Р-34, Р-36) are the row's
// details. A receipt the journal has no row for yet waits without one.
package receipt

import (
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
)

// Kind is what a receipt is, its QR code's n.
type Kind string

const (
	Purchase     Kind = "purchase"
	Refund       Kind = "refund"
	Payout       Kind = "payout"
	PayoutRefund Kind = "payout_refund"
)

// Where a receipt came from: its QR code read here, or a statement of the
// tax service's app «Проверка чеков».
const (
	SourceQR  = "qr"
	SourceFNS = "fns"
)

// IssuedAtLayout is a receipt's time as the API writes it, to the minute.
const IssuedAtLayout = "2006-01-02T15:04"

const maxDigits = 20

// incoming says whether the money comes in to the family: a refund of a
// purchase, or the seller paying out.
func (k Kind) incoming() bool { return k == Refund || k == Payout }

func (k Kind) valid() bool {
	return k == Purchase || k == Refund || k == Payout || k == PayoutRefund
}

// Item is a line of a receipt.
type Item struct {
	Name     string `json:"name"`
	Quantity string `json:"quantity"`
	Price    int64  `json:"price_minor"`
	Sum      int64  `json:"sum_minor"`
}

// Receipt is a cash receipt; OperationID is the row it completes, nil while
// it waits for one. IssuedAt is the till's time as printed, to the minute.
type Receipt struct {
	ID          uuid.UUID
	OperationID *uuid.UUID
	FN, FD      string
	FP          *string
	Kind        Kind
	IssuedAt    time.Time
	Total       int64
	Seller      *string
	SellerINN   *string
	Address     *string
	Items       []Item
	Source      string
}

var digits = regexp.MustCompile(`^[0-9]{1,20}$`)

// ErrNotFound is a receipt that is not the space's: a 404.
var ErrNotFound = fmt.Errorf("receipt: %w", pgx.ErrNoRows)

// Validate refuses a receipt no till could have printed.
func (r Receipt) Validate() error {
	switch {
	case !digits.MatchString(r.FN) || !digits.MatchString(r.FD) || r.FP != nil && !digits.MatchString(*r.FP):
		return fmt.Errorf("%w: a receipt's numbers are 1 to %d digits", family.ErrValidation, maxDigits)
	case !r.Kind.valid():
		return fmt.Errorf("%w: a receipt is a purchase, a refund, a payout or its refund", family.ErrValidation)
	case r.Total <= 0:
		return fmt.Errorf("%w: a receipt's total is above zero", family.ErrValidation)
	case r.IssuedAt.IsZero():
		return fmt.Errorf("%w: a receipt has its time", family.ErrValidation)
	}
	return nil
}

// signed is the row's amount the receipt stands for: spending below zero,
// earning above.
func (r Receipt) signed() int64 {
	if r.Kind.incoming() {
		return r.Total
	}
	return -r.Total
}
