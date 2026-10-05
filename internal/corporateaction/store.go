package corporateaction

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"babki.my/babki/internal/platform/db"
)

const (
	pgUniqueViolation    = "23505"
	instrumentEventsUniq = "instrument_events_uniq"
)

type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

const cols = `id, kind, isin, effective_on, ratio_from, ratio_to, result_isin,
	basis_share, source, source_ref, moex_secid, note, created_at, created_by`

func scan(row pgx.Row) (Event, error) {
	var e Event
	var resultISIN *string
	err := row.Scan(&e.ID, &e.Kind, &e.ISIN, &e.EffectiveOn, &e.RatioFrom, &e.RatioTo,
		&resultISIN, &e.BasisShare, &e.Source, &e.SourceRef, &e.MOEXSecID,
		&e.Note, &e.CreatedAt, &e.CreatedBy)
	if resultISIN != nil {
		e.ResultISIN = *resultISIN
	}
	return e, err
}

// nullISIN turns a split's empty result ISIN into the NULL the column's CHECK
// requires.
func nullISIN(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Create records one event; a duplicate (same paper, kind and day) is
// ErrDuplicate.
func (s *Store) Create(ctx context.Context, e Event) (Event, error) {
	created, err := scan(s.db.QueryRow(ctx, `
		INSERT INTO instrument_events (kind, isin, effective_on, ratio_from, ratio_to,
			result_isin, basis_share, source, source_ref, moex_secid, note, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+cols,
		e.Kind, e.ISIN, e.EffectiveOn, e.RatioFrom, e.RatioTo, nullISIN(e.ResultISIN),
		e.BasisShare, e.Source, e.SourceRef, e.MOEXSecID, e.Note, e.CreatedBy))
	if err != nil {
		return Event{}, wrapDuplicate(err)
	}
	return created, nil
}

// Upsert is the exchange job's write: the same event again updates the ratio
// and cached secid. It matches on (isin, kind, effective_on), never the secid,
// which can change. A hand-recorded event is not overwritten: the job counts it
// and leaves it.
func (s *Store) Upsert(ctx context.Context, e Event) (Event, bool, error) {
	row := s.db.QueryRow(ctx, `
		INSERT INTO instrument_events (kind, isin, effective_on, ratio_from, ratio_to,
			result_isin, basis_share, source, source_ref, moex_secid, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (isin, kind, effective_on) DO UPDATE
		SET ratio_from = EXCLUDED.ratio_from,
		    ratio_to   = EXCLUDED.ratio_to,
		    source_ref = EXCLUDED.source_ref,
		    moex_secid = EXCLUDED.moex_secid
		WHERE instrument_events.source = $8
		RETURNING `+cols,
		e.Kind, e.ISIN, e.EffectiveOn, e.RatioFrom, e.RatioTo, nullISIN(e.ResultISIN),
		e.BasisShare, e.Source, e.SourceRef, e.MOEXSecID, e.Note)
	stored, err := scan(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// A row is there and it is someone's own: left alone, not an error.
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	return stored, true, nil
}

func wrapDuplicate(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == instrumentEventsUniq {
		return ErrDuplicate
	}
	return err
}

// ByID returns one event, or pgx.ErrNoRows.
func (s *Store) ByID(ctx context.Context, id uuid.UUID) (Event, error) {
	return scan(s.db.QueryRow(ctx, `SELECT `+cols+` FROM instrument_events WHERE id = $1`, id))
}

// ByISIN returns one paper's events oldest first, the order they apply in.
func (s *Store) ByISIN(ctx context.Context, isin string) ([]Event, error) {
	if isin == "" {
		// An empty ISIN would match every paper without one (as instrument.ByISIN
		// refuses too).
		return nil, nil
	}
	return s.query(ctx, `SELECT `+cols+` FROM instrument_events
		WHERE isin = $1 ORDER BY effective_on, created_at, id`, isin)
}

// HasSplitOnOrBefore reports whether the registry holds a split of this paper
// effective on or before day. The broker reconciliation asks it before calling a
// twenty-to-one difference an unrecorded split; false means nobody recorded one,
// not that the difference is a split. An empty ISIN is false.
func (s *Store) HasSplitOnOrBefore(ctx context.Context, isin string, day time.Time) (bool, error) {
	if isin == "" {
		return false, nil
	}
	var exists bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM instrument_events
			WHERE isin = $1 AND kind = $2 AND effective_on <= $3
		)`, isin, KindSplit, day).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("corporateaction: look for a split of %s: %w", isin, err)
	}
	return exists, nil
}

// List returns the whole registry, newest first, for the settings screen.
func (s *Store) List(ctx context.Context) ([]Event, error) {
	return s.query(ctx, `SELECT `+cols+` FROM instrument_events
		ORDER BY effective_on DESC, created_at DESC, id`)
}

// DistinctISINs lists every paper the registry holds an event for. The sweep
// walks it; nothing else needs it.
func (s *Store) DistinctISINs(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT DISTINCT isin FROM instrument_events ORDER BY isin`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var isin string
		if err := rows.Scan(&isin); err != nil {
			return nil, err
		}
		out = append(out, isin)
	}
	return out, rows.Err()
}

// CatalogedISINs reports which ISINs the catalog has a row for, in one query
// for the whole screen. Empty strings are dropped.
func (s *Store) CatalogedISINs(ctx context.Context, isins []string) (map[string]bool, error) {
	wanted := make([]string, 0, len(isins))
	seen := map[string]bool{}
	for _, isin := range isins {
		if isin == "" || seen[isin] {
			continue
		}
		seen[isin] = true
		wanted = append(wanted, isin)
	}
	out := map[string]bool{}
	if len(wanted) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT isin FROM instruments WHERE isin = ANY($1)`, wanted)
	if err != nil {
		return nil, fmt.Errorf("corporateaction: look up which produced papers the catalog holds: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var isin string
		if err := rows.Scan(&isin); err != nil {
			return nil, err
		}
		out[isin] = true
	}
	return out, rows.Err()
}

// Delete removes a hand-recorded event; an exchange row is ErrNotEditable and
// an unknown id errNoSuchEvent, so the handler tells 400 from 404 without a
// second read.
func (s *Store) Delete(ctx context.Context, id uuid.UUID) (Event, error) {
	e, err := s.ByID(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if e.Source != SourceManual {
		return Event{}, ErrNotEditable
	}
	// The source is in the WHERE too, so a caller skipping the read cannot
	// bypass the rule.
	ct, err := s.db.Exec(ctx, `DELETE FROM instrument_events WHERE id = $1 AND source = $2`, id, SourceManual)
	if err != nil {
		return Event{}, err
	}
	if ct.RowsAffected() == 0 {
		return Event{}, errNoSuchEvent
	}
	return e, nil
}

func (s *Store) query(ctx context.Context, sql string, args ...any) ([]Event, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// holder is an account that has recorded an operation on a paper, with the
// catalog row it used: the journal names instrument ids, the registry ISINs.
type holder struct {
	spaceID      uuid.UUID
	accountID    uuid.UUID
	instrumentID uuid.UUID
}

// holders finds every (space, account, instrument) that has recorded an
// operation on this ISIN. It asks the journal, not the account list, so a sweep
// folds only holders' journals; an account that sold out is included and folds to
// zero, because only the engine knows what is held.
func (s *Store) holders(ctx context.Context, isin string) ([]holder, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT o.space_id, o.account_id, o.instrument_id
		FROM operations o
		JOIN instruments i ON i.id = o.instrument_id
		WHERE i.isin = $1
		ORDER BY o.space_id, o.account_id, o.instrument_id`, isin)
	if err != nil {
		return nil, fmt.Errorf("corporateaction: find the accounts holding %s: %w", isin, err)
	}
	defer rows.Close()
	var out []holder
	for rows.Next() {
		var h holder
		if err := rows.Scan(&h.spaceID, &h.accountID, &h.instrumentID); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// accountPaper is one paper of an account's journal the registry has something
// to say about: its ISIN and the catalog rows the journal names it by.
type accountPaper struct {
	isin          string
	instrumentIDs []uuid.UUID
}

// eventPapersOfAccount lists the papers of one account's journal that have an
// event; it runs after every hand entry.
func (s *Store) eventPapersOfAccount(ctx context.Context, spaceID, accountID uuid.UUID) ([]accountPaper, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT i.isin, o.instrument_id
		FROM operations o
		JOIN instruments i ON i.id = o.instrument_id
		WHERE o.space_id = $1 AND o.account_id = $2 AND i.isin <> ''
		  AND EXISTS (SELECT 1 FROM instrument_events e WHERE e.isin = i.isin)
		ORDER BY i.isin, o.instrument_id`, spaceID, accountID)
	if err != nil {
		return nil, fmt.Errorf("corporateaction: find the papers of account %s: %w", accountID, err)
	}
	defer rows.Close()
	var out []accountPaper
	for rows.Next() {
		var isin string
		var instrumentID uuid.UUID
		if err := rows.Scan(&isin, &instrumentID); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].isin == isin {
			out[n-1].instrumentIDs = append(out[n-1].instrumentIDs, instrumentID)
			continue
		}
		out = append(out, accountPaper{isin: isin, instrumentIDs: []uuid.UUID{instrumentID}})
	}
	return out, rows.Err()
}
