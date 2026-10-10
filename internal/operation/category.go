package operation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
)

// MaxCounterpartyRunes is the longest counterparty a row takes, in code points,
// as the column's CHECK counts them.
const MaxCounterpartyRunes = 200

// Categorizable reports whether a row can carry a category: money that came
// into or left the family's accounts, or that the bank or the state took. A
// transfer between the family's own accounts is neither a spending nor an
// earning, and a trade or an investment payout is the portfolio's, reported
// there.
func Categorizable(o Operation) bool {
	if o.TransferGroupID != nil {
		return false
	}
	return slices.Contains(categorizableTypes, string(o.Type))
}

// categorizableTypes are the types Categorizable admits, for SQL.
var categorizableTypes = []string{string(TypeDeposit), string(TypeWithdrawal), string(TypeInterest), string(TypeFee), string(TypeTax)}

func notCategorizable(o Operation) error {
	return fmt.Errorf("%w: a category goes on money that came or went (deposit, withdrawal, interest, fee, tax), not on %s or a transfer between accounts",
		family.ErrValidation, o.Type)
}

// CategoryKindOf is the kind of category a categorizable row of type t takes:
// money in earns, money out spends.
func CategoryKindOf(t Type) category.Kind { return categoryKind(t) }

// categoryKind is the kind of category a categorizable row takes: money in
// earns, money out spends.
func categoryKind(t Type) category.Kind {
	switch t {
	case TypeDeposit, TypeInterest:
		return category.KindIncome
	}
	return category.KindExpense
}

// checkCounterparty refuses a counterparty longer than the column holds.
func checkCounterparty(counterparty string) error {
	if utf8.RuneCountInString(counterparty) > MaxCounterpartyRunes {
		return fmt.Errorf("%w: counterparty must be at most %d characters", family.ErrValidation, MaxCounterpartyRunes)
	}
	return nil
}

// checkCategory holds op's category to the rules: one of the space's own, of the
// kind the row's direction takes, and not archived — unless the row already
// had it (old), so an old row keeps the category it was filed under.
func (s *Store) checkCategory(ctx context.Context, spaceID uuid.UUID, op Operation, old *Operation) error {
	if op.CategoryID == nil {
		return nil
	}
	if !Categorizable(op) {
		return notCategorizable(op)
	}
	c, err := category.NewStore(s.db).Get(ctx, spaceID, *op.CategoryID)
	if errors.Is(err, category.ErrNotFound) {
		return fmt.Errorf("%w: category_id is not one of this family's categories", family.ErrValidation)
	}
	if err != nil {
		return err
	}
	if want := categoryKind(op.Type); c.Kind != want {
		return fmt.Errorf("%w: a %s takes a category of %s, and %q is of %s", family.ErrValidation, op.Type, want, c.Name, c.Kind)
	}
	kept := old != nil && old.CategoryID != nil && *old.CategoryID == c.ID
	if c.Archived && !kept {
		return fmt.Errorf("%w: category %q is archived", family.ErrValidation, c.Name)
	}
	return nil
}

// checkMember holds op's member to the rules: whose a row is says something
// only of money that came or went (Categorizable), and only a member of the
// family can be named.
func (s *Store) checkMember(ctx context.Context, spaceID uuid.UUID, op Operation) error {
	if op.MemberID == nil {
		return nil
	}
	if !Categorizable(op) {
		return fmt.Errorf("%w: whose a row is goes on money that came or went (deposit, withdrawal, interest, fee, tax), not on %s or a transfer between accounts",
			family.ErrValidation, op.Type)
	}
	members, err := family.NewStore(s.db).ListMembers(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.ID == *op.MemberID {
			return nil
		}
	}
	return fmt.Errorf("%w: member_id is not a member of this family", family.ErrValidation)
}

// SetMember says whose a row is, or that it is the account's owner's (nil).
// Like a category, it is the family's reading of the row and works on a
// broker's too; nothing the engine reads changes.
func (s *Service) SetMember(ctx context.Context, spaceID, id uuid.UUID, memberID *uuid.UUID) (Operation, error) {
	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	st := NewStore(tx)
	old, err := st.ByID(ctx, spaceID, id)
	if err != nil {
		return Operation{}, err
	}
	op := old
	op.MemberID = memberID
	if memberID == nil && !Categorizable(old) {
		return Operation{}, notCategorizable(old)
	}
	if err := st.checkMember(ctx, spaceID, op); err != nil {
		return Operation{}, err
	}
	stored, err := scan(tx.QueryRow(ctx, `UPDATE operations SET member_id = $3 WHERE space_id = $1 AND id = $2 RETURNING `+cols,
		spaceID, id, memberID))
	if err != nil {
		return Operation{}, err
	}
	stored.Parts = old.Parts
	return stored, tx.Commit(ctx)
}

// SetCategory files a row under a category, or takes it out of one (nil). It
// works on any categorizable row, a broker's included: the category is the
// family's reading of the row, not a fact the importer reported, and an
// importer leaves alone the rows it has not changed. Nothing the engine
// reads changes, so no journal is replayed.
func (s *Service) SetCategory(ctx context.Context, spaceID, id uuid.UUID, categoryID *uuid.UUID) (Operation, error) {
	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	st := NewStore(tx)
	old, err := st.ByID(ctx, spaceID, id)
	if err != nil {
		return Operation{}, err
	}
	if !Categorizable(old) {
		return Operation{}, notCategorizable(old)
	}
	op := old
	op.CategoryID = categoryID
	if err := st.checkCategory(ctx, spaceID, op, &old); err != nil {
		return Operation{}, err
	}
	stored, err := scan(tx.QueryRow(ctx, `UPDATE operations SET category_id = $3 WHERE space_id = $1 AND id = $2 RETURNING `+cols,
		spaceID, id, categoryID))
	if err != nil {
		return Operation{}, err
	}
	stored.Parts = old.Parts
	return stored, tx.Commit(ctx)
}

// FileByRules files the rows that can take a category and have none — of one
// account, or of the whole space when accountID is nil — under what the
// space's first fitting rule names (category.Match). A broker's account is
// left alone: an unfiled row there is money between the family and its
// broker, and a rule written for a card would misread it. It answers how many
// rows it filed.
func (s *Service) FileByRules(ctx context.Context, spaceID uuid.UUID, accountID *uuid.UUID) (int, error) {
	tx, err := s.store.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cats := category.NewStore(tx)
	rules, err := cats.Rules(ctx, spaceID)
	if err != nil || len(rules) == 0 {
		return 0, err
	}
	list, err := cats.List(ctx, spaceID)
	if err != nil {
		return 0, err
	}
	byID := make(map[uuid.UUID]category.Category, len(list))
	for _, c := range list {
		byID[c.ID] = c
	}
	accounts, err := account.NewStore(tx).ListWithBalance(ctx, spaceID)
	if err != nil {
		return 0, err
	}
	var everyday []uuid.UUID
	for _, a := range accounts {
		if a.Type != account.TypeBrokerage && (accountID == nil || a.ID == *accountID) {
			everyday = append(everyday, a.ID)
		}
	}
	if len(everyday) == 0 {
		return 0, nil
	}
	rows, err := NewStore(tx).list(ctx, `SELECT `+cols+` FROM operations
		WHERE space_id = $1 AND account_id = ANY($2)
			AND category_id IS NULL AND transfer_group_id IS NULL AND type = ANY($3)`,
		spaceID, everyday, categorizableTypes)
	if err != nil {
		return 0, err
	}
	var ids, filed []uuid.UUID
	for _, op := range rows {
		if c := category.Match(rules, byID, categoryKind(op.Type), category.Text{Counterparty: op.Counterparty, Note: op.Note}); c != nil {
			ids = append(ids, op.ID)
			filed = append(filed, *c)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	ct, err := tx.Exec(ctx, `UPDATE operations o SET category_id = v.category_id
		FROM unnest($2::uuid[], $3::uuid[]) AS v(id, category_id)
		WHERE o.space_id = $1 AND o.id = v.id AND o.category_id IS NULL`, spaceID, ids, filed)
	if err != nil {
		return 0, fmt.Errorf("file by rules: %w", err)
	}
	return int(ct.RowsAffected()), tx.Commit(ctx)
}
