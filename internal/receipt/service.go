package receipt

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
)

// ErrWritten is a receipt written already: a 409.
var ErrWritten = errors.New("receipt: written already")

// Days a row may lie from its receipt's day: a bank shows a card purchase on
// the day it took the money, not always the till's.
const nearDays = 2

// maxCandidates is how many rows a receipt is offered.
const maxCandidates = 5

type journal interface {
	ByAmount(ctx context.Context, spaceID uuid.UUID, amountMinor int64, currency string, from, to time.Time) ([]operation.Operation, error)
	ByID(ctx context.Context, spaceID, id uuid.UUID) (operation.Operation, error)
}

type accounts interface {
	ListWithBalance(ctx context.Context, spaceID uuid.UUID) ([]account.WithBalance, error)
}

type categories interface {
	List(ctx context.Context, spaceID uuid.UUID) ([]category.Category, error)
	Rules(ctx context.Context, spaceID uuid.UUID) ([]category.Rule, error)
}

type splitter interface {
	SetParts(ctx context.Context, spaceID, id uuid.UUID, parts []operation.Part) (operation.Operation, error)
	ClearParts(ctx context.Context, spaceID, id uuid.UUID) (operation.Operation, error)
}

// Service keeps the receipts.
type Service struct {
	db         db.Executor
	journal    journal
	accounts   accounts
	categories categories
	splitter   splitter
}

func NewService(x db.Executor, j journal, acc accounts, cats categories, sp splitter) *Service {
	return &Service{db: x, journal: j, accounts: acc, categories: cats, splitter: sp}
}

// Lookup is where a receipt goes: the receipt as written already and the row
// it completes, or else the rows it may complete.
type Lookup struct {
	Written    *Receipt
	WrittenTo  *operation.Operation
	Candidates []operation.Operation
}

// Lookup finds the receipt by its numbers, and when it is new, the rows of
// exactly its total in roubles within nearDays of its day — on any account but
// a broker's, spending for a purchase, earning for a refund — that no receipt
// completes yet, nearest first.
func (s *Service) Lookup(ctx context.Context, spaceID uuid.UUID, r Receipt) (Lookup, error) {
	if err := r.Validate(); err != nil {
		return Lookup{}, err
	}
	var out Lookup
	written, err := s.byNumbers(ctx, spaceID, r.FN, r.FD)
	switch {
	case err == nil:
		out.Written = &written
		if written.OperationID != nil {
			op, err := s.journal.ByID(ctx, spaceID, *written.OperationID)
			if err != nil {
				return Lookup{}, err
			}
			out.WrittenTo = &op
			return out, nil
		}
		// Written and waiting for a row: the rows it may complete.
		out.Candidates, err = s.candidates(ctx, spaceID, written)
		return out, err
	case !errors.Is(err, pgx.ErrNoRows):
		return Lookup{}, err
	}
	out.Candidates, err = s.candidates(ctx, spaceID, r)
	return out, err
}

// candidates are the rows of exactly the receipt's total in roubles within
// nearDays of its day, the right way, on any account but a broker's, that no
// receipt completes yet, nearest first.
func (s *Service) candidates(ctx context.Context, spaceID uuid.UUID, r Receipt) ([]operation.Operation, error) {
	day := time.Date(r.IssuedAt.Year(), r.IssuedAt.Month(), r.IssuedAt.Day(), 0, 0, 0, 0, time.UTC)
	rows, err := s.journal.ByAmount(ctx, spaceID, r.signed(), "RUB", day.AddDate(0, 0, -nearDays), day.AddDate(0, 0, nearDays))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	list, err := s.accounts.ListWithBalance(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	broker := map[uuid.UUID]bool{}
	for _, a := range list {
		broker[a.ID] = a.Type == account.TypeBrokerage
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, op := range rows {
		ids = append(ids, op.ID)
	}
	completed, err := s.completed(ctx, spaceID, ids)
	if err != nil {
		return nil, err
	}
	var out []operation.Operation
	for _, op := range rows {
		if !broker[op.AccountID] && !completed[op.ID] && len(out) < maxCandidates {
			out = append(out, op)
		}
	}
	return out, nil
}

// Attach makes a receipt waiting for a row complete opID — the row written
// for it, or the bank's found later — and divides it by the item rules.
func (s *Service) Attach(ctx context.Context, spaceID, receiptID, opID uuid.UUID) (Receipt, error) {
	r, err := scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM receipts WHERE space_id = $1 AND id = $2`, spaceID, receiptID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Receipt{}, ErrNotFound
	}
	if err != nil {
		return Receipt{}, err
	}
	if r.OperationID != nil {
		return Receipt{}, fmt.Errorf("%w: the receipt completes a row already", family.ErrValidation)
	}
	if err := s.fits(ctx, spaceID, r, opID); err != nil {
		return Receipt{}, err
	}
	if _, err := s.db.Exec(ctx, `UPDATE receipts SET operation_id = $3 WHERE space_id = $1 AND id = $2`, spaceID, receiptID, opID); err != nil {
		return Receipt{}, fmt.Errorf("receipt: attach: %w", err)
	}
	r.OperationID = &opID
	if _, err := s.split(ctx, spaceID, opID, r.Items); err != nil {
		return Receipt{}, err
	}
	return r, nil
}

// fits refuses a row a receipt cannot complete: another family's, another
// total or currency, the wrong way, a move between accounts.
func (s *Service) fits(ctx context.Context, spaceID uuid.UUID, r Receipt, opID uuid.UUID) error {
	op, err := s.journal.ByID(ctx, spaceID, opID)
	if err != nil {
		return err
	}
	if op.Currency != "RUB" || op.AmountMinor != r.signed() || op.TransferGroupID != nil ||
		op.Type != operation.TypeWithdrawal && op.Type != operation.TypeDeposit {
		return fmt.Errorf("%w: a receipt completes a spending or an earning of its total in roubles", family.ErrValidation)
	}
	return nil
}

// Create records a receipt, completing its row when it names one: the
// family's, of exactly its total in roubles, the right way.
func (s *Service) Create(ctx context.Context, spaceID uuid.UUID, r Receipt) (Receipt, error) {
	if err := r.Validate(); err != nil {
		return Receipt{}, err
	}
	if r.OperationID != nil {
		if err := s.fits(ctx, spaceID, r, *r.OperationID); err != nil {
			return Receipt{}, err
		}
	}
	r.ID = uuid.New()
	if r.Items == nil {
		r.Items = []Item{}
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO receipts (id, space_id, operation_id, fn, fd, fp, kind, issued_at, total_minor, seller, seller_inn,
			address, items, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		r.ID, spaceID, r.OperationID, r.FN, r.FD, r.FP, r.Kind, r.IssuedAt, r.Total, r.Seller, r.SellerINN, r.Address,
		r.Items, r.Source)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Receipt{}, ErrWritten
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("receipt: create: %w", err)
	}
	return r, nil
}

// List are the receipts completing the rows ids names, or (waiting) those
// waiting for a row, newest first, at most 200.
func (s *Service) List(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID, waiting bool) ([]Receipt, error) {
	if len(ids) == 0 && !waiting {
		return []Receipt{}, nil
	}
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM receipts
		WHERE space_id = $1 AND (operation_id = ANY($2) OR $3 AND operation_id IS NULL)
		ORDER BY issued_at DESC, fn, fd LIMIT 200`, spaceID, ids, waiting)
	if err != nil {
		return nil, fmt.Errorf("receipt: list: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Receipt, error) { return scan(row) })
}

// All are the space's receipts, oldest first: for the export.
func (s *Service) All(ctx context.Context, spaceID uuid.UUID) ([]Receipt, error) {
	rows, err := s.db.Query(ctx, `SELECT `+cols+` FROM receipts WHERE space_id = $1 ORDER BY issued_at, fn, fd`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("receipt: all: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Receipt, error) { return scan(row) })
}

const cols = `id, operation_id, fn, fd, fp, kind, issued_at, total_minor, seller, seller_inn, address, items, source`

func scan(row pgx.Row) (Receipt, error) {
	var r Receipt
	err := row.Scan(&r.ID, &r.OperationID, &r.FN, &r.FD, &r.FP, &r.Kind, &r.IssuedAt, &r.Total, &r.Seller, &r.SellerINN,
		&r.Address, &r.Items, &r.Source)
	return r, err
}

func (s *Service) byNumbers(ctx context.Context, spaceID uuid.UUID, fn, fd string) (Receipt, error) {
	return scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM receipts WHERE space_id = $1 AND fn = $2 AND fd = $3`, spaceID, fn, fd))
}

// completed says which of the rows a receipt completes already.
func (s *Service) completed(ctx context.Context, spaceID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	rows, err := s.db.Query(ctx, `SELECT operation_id FROM receipts WHERE space_id = $1 AND operation_id = ANY($2)`, spaceID, ids)
	if err != nil {
		return nil, fmt.Errorf("receipt: completed: %w", err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(list))
	for _, id := range list {
		out[id] = true
	}
	return out, nil
}
