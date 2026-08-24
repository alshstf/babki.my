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

// nullISIN turns the empty result ISIN a split carries into the NULL the
// column's CHECK constraint requires. The two spellings of "no result paper"
// are the empty string in Go — where a nil string would make every reader
// dereference — and NULL in SQL, where the constraint pairs it with the kind.
func nullISIN(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Create records one event. A duplicate — same paper, same kind, same day —
// comes back as ErrDuplicate rather than as a bare constraint violation, so the
// API answers with the rule instead of an internal error.
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

// Upsert is the exchange job's write: the same event arriving again updates the
// ratio and the cached secid and leaves everything else standing.
//
// IT MATCHES ON (isin, kind, effective_on) — the unique constraint — and never
// on the secid, because the secid is what the job had to resolve to get here
// and a paper can change one (a ticker is not an identity; see migration 0020).
// Matching on the identity means the exchange correcting a ratio corrects the
// row rather than adding a second one beside it.
//
// It refuses to overwrite a HAND-RECORDED event: a person who has written down
// what a registrar told them, with a link to it, has said something the
// exchange's table does not know better. The job counts such rows and says so;
// it does not fight over them.
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
		// The WHERE on the DO UPDATE did not hold: a row is there and it is
		// somebody's own. Not an error — the caller reports it as a row left
		// alone.
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

// ByISIN returns every event recorded for one paper, oldest first — the order
// they have to be applied in, since each one acts on the holding the ones
// before it left.
func (s *Store) ByISIN(ctx context.Context, isin string) ([]Event, error) {
	if isin == "" {
		// Every instrument with no ISIN would otherwise match at once, and the
		// answer would be a pile of events belonging to papers that have
		// nothing to do with each other. The same refusal instrument.ByISIN
		// makes, for the same reason.
		return nil, nil
	}
	return s.query(ctx, `SELECT `+cols+` FROM instrument_events
		WHERE isin = $1 ORDER BY effective_on, created_at, id`, isin)
}

// HasSplitOnOrBefore reports whether the registry holds a split of this paper
// effective on or before the given day.
//
// IT IS A QUESTION ABOUT THE REGISTRY, NOT ABOUT ANY JOURNAL. The caller is the
// broker reconciliation, which has found a difference that looks exactly like an
// unrecorded split — the broker holding twenty of what the journal holds one of
// — and wants to say so only when the registry cannot already account for it.
// A false answer therefore means "nobody here has recorded such an event", which
// is a question for a person; it does not mean the difference IS a split.
//
// ON OR BEFORE, because an event dated after the check could not have affected
// today's holding, and pointing at it would send the reader to a row that
// explains nothing. An empty ISIN is false rather than a match on every paper
// with no ISIN, the same refusal ByISIN makes.
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

// List returns the whole registry, newest event first — what the settings
// screen shows. Small by nature: one row per corporate action of every paper
// anyone here holds, plus whatever the exchange published.
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

// Delete removes a hand-recorded event. An exchange row is refused by name
// (ErrNotEditable) and an unknown id by errNoSuchEvent, so the handler can tell
// a 400 from a 404 without a second read.
func (s *Store) Delete(ctx context.Context, id uuid.UUID) (Event, error) {
	e, err := s.ByID(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if e.Source != SourceManual {
		return Event{}, ErrNotEditable
	}
	// The source is part of the WHERE and not merely of the read above: between
	// the two, nothing in this program can change a row's source, but writing
	// the rule where the deletion happens costs one comparison and means the
	// rule cannot be bypassed by a future caller that skips the read.
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

// holder is one account that has ever recorded an operation on a paper, and the
// catalog row it recorded it against.
//
// THE CATALOG ROW IS PART OF THE ANSWER because a journal names an instrument
// id and the registry names an ISIN: two catalog rows can carry one ISIN only
// if a database predates migration 0020, but an account can perfectly well hold
// one paper under a row this instance created and another under a row the
// importer created, and the split has to reach the rows the journal actually
// uses.
type holder struct {
	spaceID      uuid.UUID
	accountID    uuid.UUID
	instrumentID uuid.UUID
}

// holders finds every (space, account, instrument) that has ever recorded an
// operation on the paper this ISIN names.
//
// IT ASKS THE JOURNAL AND NOT THE ACCOUNT LIST, and that is what keeps a sweep
// cheap: an instance with forty accounts and one holder of Amazon folds one
// journal rather than forty. It over-answers on purpose — an account that
// bought the paper and sold it all is in this list and folds to a holding of
// zero, which the materialization then declines to write a split for — because
// the alternative is deciding what is held with a query rather than with the
// engine, and the engine is the only thing that knows.
//
// Rows the registry itself wrote are included in the count of "has ever
// recorded", which is harmless: a registry row exists only where an operation
// already did.
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

// isinsOfAccount lists the ISINs an account's journal touches. It answers the
// question the other way round from holders, for the trigger that fires after
// somebody writes an operation by hand: what papers might this account now need
// events materialized for.
func (s *Store) isinsOfAccount(ctx context.Context, spaceID, accountID uuid.UUID) ([]string, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT i.isin
		FROM operations o
		JOIN instruments i ON i.id = o.instrument_id
		WHERE o.space_id = $1 AND o.account_id = $2 AND i.isin <> ''
		ORDER BY i.isin`, spaceID, accountID)
	if err != nil {
		return nil, fmt.Errorf("corporateaction: find the papers of account %s: %w", accountID, err)
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
