package receipt

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/category"
	"babki.my/babki/internal/operation"
)

// ImportResult says what a statement brought: the receipts found in it;
// those that completed a row at once (the one row of their total near their
// day); those waiting for a row (none such, or several); the known ones that
// got their seller and items now (read off a QR code before); the known ones
// with nothing new; and the rows split by their items' rules.
type ImportResult struct {
	Found, Attached, Waiting, Enriched, Known, Split int
}

// Import records the receipts of a statement (decision Р-34): a new one
// completes the one row it can, or waits; a known one gets what it lacked.
// A row completed by a receipt with items is split across the categories the
// item rules name (decision Р-36).
func (s *Service) Import(ctx context.Context, spaceID uuid.UUID, receipts []Receipt) (ImportResult, error) {
	res := ImportResult{Found: len(receipts)}
	for _, r := range receipts {
		if err := r.Validate(); err != nil {
			return ImportResult{}, err
		}
		known, err := s.byNumbers(ctx, spaceID, r.FN, r.FD)
		switch {
		case err == nil:
			if !s.richer(known, r) {
				res.Known++
				continue
			}
			if err := s.enrich(ctx, spaceID, known.ID, r); err != nil {
				return ImportResult{}, err
			}
			res.Enriched++
			if known.OperationID != nil {
				split, err := s.split(ctx, spaceID, *known.OperationID, r.Items)
				if err != nil {
					return ImportResult{}, err
				}
				if split {
					res.Split++
				}
			}
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return ImportResult{}, err
		}
		rows, err := s.candidates(ctx, spaceID, r)
		if err != nil {
			return ImportResult{}, err
		}
		if len(rows) == 1 {
			r.OperationID = &rows[0].ID
		}
		if _, err := s.Create(ctx, spaceID, r); err != nil {
			return ImportResult{}, err
		}
		if r.OperationID == nil {
			res.Waiting++
			continue
		}
		res.Attached++
		split, err := s.split(ctx, spaceID, *r.OperationID, r.Items)
		if err != nil {
			return ImportResult{}, err
		}
		if split {
			res.Split++
		}
	}
	return res, nil
}

// Resplit divides the row a receipt completes by the item rules as they are
// now, over whatever split it had: asked for after a new rule is learned from
// one of its lines. False when the rules name no other category for its items.
func (s *Service) Resplit(ctx context.Context, spaceID, receiptID uuid.UUID) (bool, error) {
	r, err := scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM receipts WHERE space_id = $1 AND id = $2`, spaceID, receiptID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if r.OperationID == nil {
		return false, nil
	}
	if _, err := s.splitter.ClearParts(ctx, spaceID, *r.OperationID); err != nil {
		return false, err
	}
	return s.split(ctx, spaceID, *r.OperationID, r.Items)
}

// richer says whether the statement's copy of a known receipt brings what it
// lacks: the seller or the items.
func (s *Service) richer(known, r Receipt) bool {
	return len(known.Items) == 0 && len(r.Items) > 0 || known.Seller == nil && r.Seller != nil
}

// enrich writes the seller, the address and the items over a known
// receipt's, and its fiscal sign when it had none.
func (s *Service) enrich(ctx context.Context, spaceID, id uuid.UUID, r Receipt) error {
	if r.Items == nil {
		r.Items = []Item{}
	}
	_, err := s.db.Exec(ctx, `
		UPDATE receipts SET seller = COALESCE($3, seller), seller_inn = COALESCE($4, seller_inn),
			address = COALESCE($5, address), items = CASE WHEN jsonb_array_length($6::jsonb) > 0 THEN $6::jsonb ELSE items END,
			fp = COALESCE(fp, $7)
		WHERE space_id = $1 AND id = $2`, spaceID, id, r.Seller, r.SellerINN, r.Address, r.Items, r.FP)
	if err != nil {
		return fmt.Errorf("receipt: enrich: %w", err)
	}
	return nil
}

// split divides a spending its receipt completes across the categories the
// item rules give its items (category.FieldItem, decision Р-36): the items no
// rule names, and what the receipt's total holds beyond its items, stay with
// the row's own category. Nothing happens to a row split already, one with
// no category, a refund, or when no item rule names another category.
func (s *Service) split(ctx context.Context, spaceID, opID uuid.UUID, items []Item) (bool, error) {
	if len(items) == 0 {
		return false, nil
	}
	op, err := s.journal.ByID(ctx, spaceID, opID)
	if err != nil {
		return false, err
	}
	if op.Type != operation.TypeWithdrawal || op.CategoryID == nil || len(op.Parts) > 0 {
		return false, nil
	}
	rules, err := s.categories.Rules(ctx, spaceID)
	if err != nil {
		return false, err
	}
	var byItem []category.Rule
	for _, r := range rules {
		if r.Field == category.FieldItem {
			byItem = append(byItem, r)
		}
	}
	if len(byItem) == 0 {
		return false, nil
	}
	list, err := s.categories.List(ctx, spaceID)
	if err != nil {
		return false, err
	}
	byID := make(map[uuid.UUID]category.Category, len(list))
	for _, c := range list {
		byID[c.ID] = c
	}
	var order []uuid.UUID
	sums := map[uuid.UUID]int64{}
	var moved int64
	for _, it := range items {
		c := category.Match(byItem, byID, category.KindExpense, category.Text{Item: it.Name})
		if c == nil || *c == *op.CategoryID || it.Sum <= 0 {
			continue
		}
		if _, seen := sums[*c]; !seen {
			order = append(order, *c)
		}
		sums[*c] += it.Sum
		moved += it.Sum
	}
	rest := -op.AmountMinor - moved
	if len(order) == 0 || rest < 0 {
		return false, nil
	}
	var parts []operation.Part
	if rest > 0 {
		parts = append(parts, operation.Part{CategoryID: *op.CategoryID, Amount: rest})
	}
	for _, c := range order {
		parts = append(parts, operation.Part{CategoryID: c, Amount: sums[c]})
	}
	if len(parts) < 2 {
		return false, nil
	}
	if _, err := s.splitter.SetParts(ctx, spaceID, opID, parts); err != nil {
		return false, err
	}
	return true, nil
}
