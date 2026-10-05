package operation

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/apitypes"
	"babki.my/babki/internal/platform/httpjson"
	"babki.my/babki/internal/platform/money"
)

// A tax withheld abroad as a person stated it from the broker's statement
// (decision Р-14): it takes the place of the estimate for that payment. Stored
// per payment — account, paper, day — so an import rewriting the row keeps it.

// paymentKey names one dividend payment.
type paymentKey struct {
	accountID, instrumentID uuid.UUID
	paidOn                  string // YYYY-MM-DD
}

func paymentOf(o Operation) paymentKey {
	return paymentKey{accountID: o.AccountID, instrumentID: *o.InstrumentID, paidOn: o.OccurredOn.Format(time.DateOnly)}
}

// StatedWithheld is one stated tax: on the dividend of InstrumentID paid to
// AccountID on PaidOn.
type StatedWithheld struct {
	AccountID, InstrumentID uuid.UUID
	PaidOn                  time.Time
	TaxMinor                int64
}

func (w StatedWithheld) key() paymentKey {
	return paymentKey{accountID: w.AccountID, instrumentID: w.InstrumentID, paidOn: w.PaidOn.Format(time.DateOnly)}
}

// StatedWithheld is every stated tax on the given accounts' payments, oldest
// payment first.
func (s *Store) StatedWithheld(ctx context.Context, spaceID uuid.UUID, accountIDs []uuid.UUID) ([]StatedWithheld, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT account_id, instrument_id, paid_on, tax_minor FROM dividend_withheld_stated
		WHERE space_id = $1 AND account_id = ANY($2)
		ORDER BY paid_on, instrument_id`, spaceID, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("operation: read stated withholdings: %w", err)
	}
	defer rows.Close()
	var out []StatedWithheld
	for rows.Next() {
		var w StatedWithheld
		if err := rows.Scan(&w.AccountID, &w.InstrumentID, &w.PaidOn, &w.TaxMinor); err != nil {
			return nil, fmt.Errorf("operation: read stated withholdings: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) stateWithheld(ctx context.Context, spaceID uuid.UUID, k paymentKey, taxMinor int64) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO dividend_withheld_stated (space_id, account_id, instrument_id, paid_on, tax_minor)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (account_id, instrument_id, paid_on)
		DO UPDATE SET tax_minor = EXCLUDED.tax_minor, stated_at = now()`,
		spaceID, k.accountID, k.instrumentID, k.paidOn, taxMinor)
	return err
}

func (s *Store) clearWithheld(ctx context.Context, spaceID uuid.UUID, k paymentKey) error {
	_, err := s.db.Exec(ctx, `
		DELETE FROM dividend_withheld_stated
		WHERE space_id = $1 AND account_id = $2 AND instrument_id = $3 AND paid_on = $4`,
		spaceID, k.accountID, k.instrumentID, k.paidOn)
	return err
}

// StateWithheld records taxMinor as the tax withheld abroad from the payment
// operation id belongs to; nil clears it, bringing the estimate back. Only a
// dividend on a paper has one. Any row may carry it, an imported one too: it
// describes the payment, not the row.
func (s *Service) StateWithheld(ctx context.Context, spaceID, id uuid.UUID, taxMinor *int64) error {
	o, err := s.store.ByID(ctx, spaceID, id)
	if err != nil {
		return err
	}
	if o.Type != TypeDividend || o.InstrumentID == nil {
		return fmt.Errorf("%w: only a dividend on a paper has a tax withheld abroad", family.ErrValidation)
	}
	if taxMinor == nil {
		return s.store.clearWithheld(ctx, spaceID, paymentOf(o))
	}
	if *taxMinor < 0 || *taxMinor > money.MaxAmountMinor {
		return fmt.Errorf("%w: tax_minor must be from 0 to %d", family.ErrValidation, money.MaxAmountMinor)
	}
	return s.store.stateWithheld(ctx, spaceID, paymentOf(o), *taxMinor)
}

func (h *Handler) handleStateWithheld(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathOperationID(w, r)
	if !ok {
		return
	}
	var req apitypes.StateWithheldRequest
	if httpjson.Decode(w, r, &req) != nil {
		return
	}
	if err := h.svc.StateWithheld(r.Context(), p.SpaceID, id, &req.TaxMinor); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleClearWithheld(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	id, ok := pathOperationID(w, r)
	if !ok {
		return
	}
	if err := h.svc.StateWithheld(r.Context(), p.SpaceID, id, nil); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
