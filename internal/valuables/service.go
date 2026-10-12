package valuables

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/family"
	"babki.my/babki/internal/marketdata"
	"babki.my/babki/internal/platform/db"
)

// ErrNotFound: no such piece on the account.
var ErrNotFound = fmt.Errorf("valuables: %w", pgx.ErrNoRows)

type accounts interface {
	ByID(ctx context.Context, spaceID, id uuid.UUID) (account.WithBalance, error)
}

type prices interface {
	MetalPriceOn(ctx context.Context, metal marketdata.Metal, on time.Time) (marketdata.MetalPrice, error)
}

// Service keeps storages, their pieces and the bank's prices, and values
// them.
type Service struct {
	db       db.Executor
	accounts accounts
	prices   prices
	now      func() time.Time
	// For Worths (WithWorth).
	spaces  spaces
	history history
	rates   marketdata.RateSource
	// For a sale's money (WithJournal).
	journal    journal
	categories categories
}

func NewService(x db.Executor, acc accounts, p prices) *Service {
	return &Service{db: x, accounts: acc, prices: p, now: time.Now}
}

// Valued is a piece and its value.
type Valued struct {
	Item
	Value Value
}

// Holding is what a storage holds and is worth: in roubles by the central
// bank's price (Market) and as it would sell now (Sell) — nil when a piece
// cannot be valued yet, a part of a sum being worse than none — and the
// pieces it sold, each with its gain and tax.
type Holding struct {
	Storage      Storage
	Items        []Valued
	Sold         []Sold
	BankPrices   []BankPrice
	Market, Sell *int64
}

// Sold is a piece sold: its gain over what it cost, whether it sold free of
// tax, and the tax it brings on its own (SaleTax).
type Sold struct {
	Item
	Gain int64
	Free bool
	Tax  int64
}

// storageAccount is the space's account, refused unless it keeps valuables.
func (s *Service) storageAccount(ctx context.Context, spaceID, accountID uuid.UUID) error {
	a, err := s.accounts.ByID(ctx, spaceID, accountID)
	if err != nil {
		return err
	}
	if a.Type != account.TypeValuables {
		return fmt.Errorf("%w: bars and coins are kept on a valuables account", family.ErrValidation)
	}
	return nil
}

// Of is what the account holds and is worth today.
func (s *Service) Of(ctx context.Context, spaceID, accountID uuid.UUID) (Holding, error) {
	if err := s.storageAccount(ctx, spaceID, accountID); err != nil {
		return Holding{}, err
	}
	return s.holding(ctx, spaceID, accountID, s.now())
}

// stored is a storage as written: its terms, the bank's prices and its
// pieces, not yet valued.
type stored struct {
	Storage    Storage
	BankPrices []BankPrice
	Items      []Item
}

// Entered is what the family wrote in for a valuables account — its terms,
// the bank's prices and its pieces, nothing valued — for the space's export;
// ok is false for an account of another type.
func (s *Service) Entered(ctx context.Context, spaceID, accountID uuid.UUID) (Storage, []BankPrice, []Item, bool, error) {
	a, err := s.accounts.ByID(ctx, spaceID, accountID)
	if err != nil || a.Type != account.TypeValuables {
		return Storage{}, nil, nil, false, err
	}
	st, err := s.load(ctx, accountID)
	if err != nil {
		return Storage{}, nil, nil, true, err
	}
	return st.Storage, st.BankPrices, st.Items, true, nil
}

func (s *Service) load(ctx context.Context, accountID uuid.UUID) (stored, error) {
	h := stored{Storage: Storage{Place: PlaceHome}, BankPrices: []BankPrice{}}
	var period *string
	err := s.db.QueryRow(ctx, `
		SELECT place, bank, until, fee_minor, fee_period, buyback_percent
		FROM valuables_storages WHERE account_id = $1`, accountID).
		Scan(&h.Storage.Place, &h.Storage.Bank, &h.Storage.Until, &h.Storage.FeeMinor, &period, &h.Storage.Buyback)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return stored{}, fmt.Errorf("valuables: storage: %w", err)
	}
	if period != nil {
		p := FeePeriod(*period)
		h.Storage.FeePeriod = &p
	}
	rows, err := s.db.Query(ctx, `SELECT metal, price, on_date FROM valuables_bank_prices WHERE account_id = $1 ORDER BY metal`, accountID)
	if err != nil {
		return stored{}, fmt.Errorf("valuables: bank prices: %w", err)
	}
	for rows.Next() {
		var b BankPrice
		if err := rows.Scan(&b.Metal, &b.Price, &b.On); err != nil {
			rows.Close()
			return stored{}, err
		}
		h.BankPrices = append(h.BankPrices, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return stored{}, err
	}
	if h.Items, err = s.items(ctx, accountID); err != nil {
		return stored{}, err
	}
	return h, nil
}

func (s *Service) holding(ctx context.Context, spaceID, accountID uuid.UUID, today time.Time) (Holding, error) {
	st, err := s.load(ctx, accountID)
	if err != nil {
		return Holding{}, err
	}
	h := Holding{Storage: st.Storage, BankPrices: st.BankPrices, Items: []Valued{}, Sold: []Sold{}}
	rules, err := s.rules(ctx, spaceID)
	if err != nil {
		return Holding{}, err
	}
	cbr := map[marketdata.Metal]*marketdata.MetalPrice{}
	var market, sell int64
	whole := true
	for _, it := range st.Items {
		if it.SoldOn != nil {
			gain, free, tax := SaleTax(it, rules)
			h.Sold = append(h.Sold, Sold{Item: it, Gain: gain, Free: free, Tax: tax})
			continue
		}
		p, seen := cbr[it.Metal]
		if !seen {
			got, err := s.prices.MetalPriceOn(ctx, it.Metal, today)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return Holding{}, err
			default:
				p = &got
			}
			cbr[it.Metal] = p
		}
		v := Valuing(it, p, h.Storage, h.BankPrices, today)
		h.Items = append(h.Items, Valued{Item: it, Value: v})
		if v.Market == nil || v.Sell == nil {
			whole = false
			continue
		}
		market += *v.Market
		sell += *v.Sell
	}
	if whole {
		h.Market, h.Sell = &market, &sell
	}
	return h, nil
}

const itemCols = `id, account_id, kind, metal, weight, fineness, maker, foreign_made, serial, certificate, sealed,
	bought_on, cost_minor, currency, owner_user_id, note`

// soldCols are a piece's sale, read beside itemCols.
const soldCols = `sold_on, sold_minor, sale_operation_id`

func (s *Service) items(ctx context.Context, accountID uuid.UUID) ([]Item, error) {
	rows, err := s.db.Query(ctx, `SELECT `+itemCols+`, `+soldCols+` FROM valuables_items WHERE account_id = $1 ORDER BY bought_on, created_at`, accountID)
	if err != nil {
		return nil, fmt.Errorf("valuables: items: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.AccountID, &it.Kind, &it.Metal, &it.Weight, &it.Fineness, &it.Maker, &it.ForeignMade,
			&it.Serial, &it.Certificate, &it.Sealed, &it.BoughtOn, &it.CostMinor, &it.Currency, &it.OwnerUserID, &it.Note,
			&it.SoldOn, &it.SoldMinor, &it.SaleOperationID); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetStorage states the storage's terms anew.
func (s *Service) SetStorage(ctx context.Context, spaceID, accountID uuid.UUID, st Storage) (Holding, error) {
	if err := s.storageAccount(ctx, spaceID, accountID); err != nil {
		return Holding{}, err
	}
	if err := validStorage(st); err != nil {
		return Holding{}, err
	}
	var period *string
	if st.FeePeriod != nil {
		p := string(*st.FeePeriod)
		period = &p
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO valuables_storages (account_id, place, bank, until, fee_minor, fee_period, buyback_percent)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (account_id) DO UPDATE SET place = EXCLUDED.place, bank = EXCLUDED.bank, until = EXCLUDED.until,
			fee_minor = EXCLUDED.fee_minor, fee_period = EXCLUDED.fee_period, buyback_percent = EXCLUDED.buyback_percent`,
		accountID, st.Place, strings.TrimSpace(st.Bank), st.Until, st.FeeMinor, period, st.Buyback)
	if err != nil {
		return Holding{}, fmt.Errorf("valuables: set storage: %w", err)
	}
	return s.holding(ctx, spaceID, accountID, s.now())
}

func validStorage(st Storage) error {
	switch {
	case !slices.Contains([]Place{PlaceHome, PlaceSafeBox, PlaceCustody, PlaceOther}, st.Place):
		return fmt.Errorf("%w: the place is home, safe_box, custody or other", family.ErrValidation)
	case (st.FeeMinor == nil) != (st.FeePeriod == nil):
		return fmt.Errorf("%w: a fee comes with its period", family.ErrValidation)
	case st.FeeMinor != nil && *st.FeeMinor < 0:
		return fmt.Errorf("%w: a fee is not below zero", family.ErrValidation)
	case st.FeePeriod != nil && *st.FeePeriod != FeeMonth && *st.FeePeriod != FeeYear:
		return fmt.Errorf("%w: a fee is paid by month or by year", family.ErrValidation)
	case st.Buyback != nil && (st.Buyback.IsNegative() || !st.Buyback.LessThan(decimal.NewFromInt(100)) || !st.Buyback.Equal(st.Buyback.Round(2))):
		return fmt.Errorf("%w: the buyback is 0 to 100 percent, to hundredths", family.ErrValidation)
	}
	return nil
}

// AddItem puts a new piece on the account.
func (s *Service) AddItem(ctx context.Context, spaceID, accountID uuid.UUID, it Item) (Holding, error) {
	if err := s.storageAccount(ctx, spaceID, accountID); err != nil {
		return Holding{}, err
	}
	if err := s.validItem(it); err != nil {
		return Holding{}, err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO valuables_items (`+itemCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		uuid.New(), accountID, it.Kind, it.Metal, it.Weight, it.Fineness, strings.TrimSpace(it.Maker), it.ForeignMade,
		strings.TrimSpace(it.Serial), it.Certificate, it.Sealed, it.BoughtOn, it.CostMinor, it.Currency, it.OwnerUserID, strings.TrimSpace(it.Note))
	if err != nil {
		return Holding{}, writeErr("add item", err)
	}
	return s.holding(ctx, spaceID, accountID, s.now())
}

// UpdateItem states a piece anew.
func (s *Service) UpdateItem(ctx context.Context, spaceID, accountID, itemID uuid.UUID, it Item) (Holding, error) {
	if err := s.storageAccount(ctx, spaceID, accountID); err != nil {
		return Holding{}, err
	}
	if err := s.validItem(it); err != nil {
		return Holding{}, err
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE valuables_items SET kind = $3, metal = $4, weight = $5, fineness = $6, maker = $7, foreign_made = $8, serial = $9,
			certificate = $10, sealed = $11, bought_on = $12, cost_minor = $13, currency = $14, owner_user_id = $15, note = $16
		WHERE id = $1 AND account_id = $2`,
		itemID, accountID, it.Kind, it.Metal, it.Weight, it.Fineness, strings.TrimSpace(it.Maker), it.ForeignMade,
		strings.TrimSpace(it.Serial), it.Certificate, it.Sealed, it.BoughtOn, it.CostMinor, it.Currency, it.OwnerUserID, strings.TrimSpace(it.Note))
	if err != nil {
		return Holding{}, writeErr("update item", err)
	}
	if tag.RowsAffected() == 0 {
		return Holding{}, ErrNotFound
	}
	return s.holding(ctx, spaceID, accountID, s.now())
}

// DeleteItem takes a piece off the account — written in by mistake; a sale
// is its own operation (plan step 4).
func (s *Service) DeleteItem(ctx context.Context, spaceID, accountID, itemID uuid.UUID) (Holding, error) {
	if err := s.storageAccount(ctx, spaceID, accountID); err != nil {
		return Holding{}, err
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM valuables_items WHERE id = $1 AND account_id = $2`, itemID, accountID)
	if err != nil {
		return Holding{}, fmt.Errorf("valuables: delete item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Holding{}, ErrNotFound
	}
	return s.holding(ctx, spaceID, accountID, s.now())
}

// SetBankPrice writes in what the family's bank buys a gram of the metal
// back for, as of a day.
func (s *Service) SetBankPrice(ctx context.Context, spaceID, accountID uuid.UUID, b BankPrice) (Holding, error) {
	if err := s.storageAccount(ctx, spaceID, accountID); err != nil {
		return Holding{}, err
	}
	switch {
	case !slices.Contains(marketdata.Metals, b.Metal):
		return Holding{}, fmt.Errorf("%w: the metal is gold, silver, platinum or palladium", family.ErrValidation)
	case !b.Price.IsPositive():
		return Holding{}, fmt.Errorf("%w: a price is above zero", family.ErrValidation)
	case b.On.After(s.now()):
		return Holding{}, fmt.Errorf("%w: a price is not from the future", family.ErrValidation)
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO valuables_bank_prices (account_id, metal, price, on_date) VALUES ($1, $2, $3, $4)
		ON CONFLICT (account_id, metal) DO UPDATE SET price = EXCLUDED.price, on_date = EXCLUDED.on_date`,
		accountID, b.Metal, b.Price.Round(4), b.On)
	if err != nil {
		return Holding{}, fmt.Errorf("valuables: set bank price: %w", err)
	}
	return s.holding(ctx, spaceID, accountID, s.now())
}

var currencyCode = func(c string) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func (s *Service) validItem(it Item) error {
	switch {
	case it.Kind != KindBar && it.Kind != KindCoin:
		return fmt.Errorf("%w: a piece is a bar or a coin", family.ErrValidation)
	case !slices.Contains(marketdata.Metals, it.Metal):
		return fmt.Errorf("%w: the metal is gold, silver, platinum or palladium", family.ErrValidation)
	case !it.Weight.IsPositive() || !it.Weight.Equal(it.Weight.Round(4)):
		return fmt.Errorf("%w: the weight is above zero grams, to ten-thousandths", family.ErrValidation)
	case !it.Fineness.IsPositive() || it.Fineness.GreaterThan(decimal.NewFromInt(1000)) || !it.Fineness.Equal(it.Fineness.Round(1)):
		return fmt.Errorf("%w: the fineness is up to 1000 thousandths, to tenths (999.9)", family.ErrValidation)
	case it.CostMinor < 0:
		return fmt.Errorf("%w: what it cost is not below zero", family.ErrValidation)
	case !currencyCode(it.Currency):
		return fmt.Errorf("%w: a currency is three capital letters", family.ErrValidation)
	case it.BoughtOn.IsZero() || it.BoughtOn.After(s.now()):
		return fmt.Errorf("%w: the day it was bought is known and not in the future", family.ErrValidation)
	}
	return nil
}

// writeErr is a write's failure: a person named who is no user of this
// instance is the caller's mistake.
func writeErr(what string, err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23503" && pg.ConstraintName == "valuables_items_owner_user_id_fkey" {
		return fmt.Errorf("%w: owner_user_id does not name a user", family.ErrValidation)
	}
	return fmt.Errorf("valuables: %s: %w", what, err)
}

// rules are the space's tax rules, Russia's for a service built without the
// spaces (WithWorth).
func (s *Service) rules(ctx context.Context, spaceID uuid.UUID) (family.TaxRules, error) {
	if s.spaces == nil {
		return family.TaxRulesFor(family.DefaultTaxResidency), nil
	}
	sp, err := s.spaces.SpaceByID(ctx, spaceID)
	if err != nil {
		return family.TaxRules{}, err
	}
	return sp.CostBasisRules(), nil
}
