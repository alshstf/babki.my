package allocation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
)

// SavedPlan is a what-if plan the family kept (decision Р-48, step 3): its
// operations as typed, valued again whenever it is opened.
type SavedPlan struct {
	ID      uuid.UUID
	Name    string
	SavedAt time.Time
	Ops     []Op
}

// MaxPlanName bounds a saved plan's name; MaxPlanOps its operations, as the
// what-if bounds them.
const (
	MaxPlanName = 100
	MaxPlanOps  = 50
)

// ErrPlanNotFound: no such saved plan in the space.
var ErrPlanNotFound = fmt.Errorf("allocation: plan: %w", pgx.ErrNoRows)

// savedOp is an operation as the table keeps it.
type savedOp struct {
	Kind       OpKind     `json:"kind"`
	Account    uuid.UUID  `json:"account_id"`
	To         *uuid.UUID `json:"to_account_id,omitempty"`
	Instrument *uuid.UUID `json:"instrument_id,omitempty"`
	Currency   string     `json:"currency"`
	Minor      int64      `json:"amount_minor"`
}

// Plans are the space's saved plans, the latest saved first.
func (s *Service) Plans(ctx context.Context, spaceID uuid.UUID) ([]SavedPlan, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, saved_at, operations FROM allocation_plans
		WHERE space_id = $1 ORDER BY saved_at DESC, name`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("allocation: plans: %w", err)
	}
	defer rows.Close()
	var out []SavedPlan
	for rows.Next() {
		var p SavedPlan
		var raw []byte
		if err := rows.Scan(&p.ID, &p.Name, &p.SavedAt, &raw); err != nil {
			return nil, fmt.Errorf("allocation: plans: %w", err)
		}
		var ops []savedOp
		if err := json.Unmarshal(raw, &ops); err != nil {
			return nil, fmt.Errorf("allocation: plan %s: %w", p.ID, err)
		}
		for _, o := range ops {
			p.Ops = append(p.Ops, Op(o))
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SavePlan keeps a plan under its name; one saved under the same name before
// is replaced, its day too.
func (s *Service) SavePlan(ctx context.Context, spaceID uuid.UUID, name string, ops []Op) error {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxPlanName {
		return fmt.Errorf("%w: a plan's name is 1 to %d characters", family.ErrValidation, MaxPlanName)
	}
	if len(ops) == 0 || len(ops) > MaxPlanOps {
		return fmt.Errorf("%w: a plan has 1 to %d operations", family.ErrValidation, MaxPlanOps)
	}
	kept := make([]savedOp, 0, len(ops))
	for _, op := range ops {
		if !wellFormed(op) {
			return errPlan
		}
		kept = append(kept, savedOp(op))
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO allocation_plans (space_id, name, operations) VALUES ($1, $2, $3)
		ON CONFLICT (space_id, name) DO UPDATE SET operations = EXCLUDED.operations, saved_at = now()`,
		spaceID, name, raw); err != nil {
		return fmt.Errorf("allocation: save plan: %w", err)
	}
	return nil
}

// DeletePlan forgets a saved plan.
func (s *Service) DeletePlan(ctx context.Context, spaceID, id uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM allocation_plans WHERE id = $1 AND space_id = $2`, id, spaceID)
	if err != nil {
		return fmt.Errorf("allocation: delete plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrPlanNotFound
	}
	return nil
}

// wellFormed is an operation the what-if could take, its accounts and paper
// aside: they are looked up when it is valued.
func wellFormed(op Op) bool {
	if op.Minor <= 0 || !currencyCode.MatchString(op.Currency) {
		return false
	}
	switch op.Kind {
	case OpBuy, OpSell:
		return op.Instrument != nil && op.To == nil
	case OpDeposit, OpWithdraw:
		return op.Instrument == nil && op.To == nil
	case OpTransfer:
		return op.Instrument == nil && op.To != nil && *op.To != op.Account
	default:
		return false
	}
}
