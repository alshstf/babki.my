package tinvest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrRowNotInLink is returned when a content key named for an explanation is
// not one of that link's mirror rows. It is a 404 rather than a 400: the
// request is well formed and names a row this connection does not hold.
var ErrRowNotInLink = errors.New("tinvest: no mirror row of this link carries that content key")

// ErrRowAlreadyExplained is returned when a mirror row named for an
// explanation already has one. A 409: the row is real, the request is well
// formed, and the conflict is with a record that exists.
var ErrRowAlreadyExplained = errors.New("tinvest: this mirror row is already accounted for by hand")

// ErrExplanationNotFound is returned when no explanation of this space carries
// the id asked for.
var ErrExplanationNotFound = errors.New("tinvest: explanation not found")

// RowExplanation is the manual operation that accounts for one mirror row,
// as a listing shows it.
//
// It carries the operation's DATE AND TYPE and not its amount, because it is
// a caption on the mirror row rather than a copy of the journal entry: what a
// reader needs here is which entry to look at, and the journal is where that
// entry is read. Copying the figures would be a second statement of the same
// money, and the two would part company the first time the operation is
// edited.
type RowExplanation struct {
	ID          uuid.UUID
	OperationID uuid.UUID
	// OperationOn is the journal date of the operation, i.e. the day the owner
	// says the event happened, which need not be the broker's own date for
	// either of the rows explained — the two halves of a fund's redemption are
	// a fortnight apart and one operation stands for both.
	OperationOn   time.Time
	OperationType string
}

// Explanation is one row of the explanations table, with the connection it
// belongs to for the authorization the caller has to do.
type Explanation struct {
	ID           uuid.UUID
	LinkID       uuid.UUID
	ConnectionID uuid.UUID
	SpaceID      uuid.UUID
	ContentKey   string
	OperationID  uuid.UUID
	CreatedAt    time.Time
}

// ExplainedKeysByLink is the set of this link's mirror rows the owner has
// accounted for by hand — the one question the projection asks of this table,
// asked once per link and answered by content key, which is what a mirror row
// is identified by.
func (s *Store) ExplainedKeysByLink(ctx context.Context, linkID uuid.UUID) (map[string]bool, error) {
	rows, err := s.db.Query(ctx,
		`SELECT content_key FROM tinvest_mirror_explanations WHERE link_id = $1`, linkID)
	if err != nil {
		return nil, fmt.Errorf("tinvest: list the explained rows of a link: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("tinvest: list the explained rows of a link: %w", err)
		}
		out[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tinvest: list the explained rows of a link: %w", err)
	}
	return out, nil
}

// attachExplanations fills ExplainedBy on every row that has one.
//
// A SECOND QUERY RATHER THAN A JOIN, so that the mirror's own columns are read
// by the one scanner every other mirror query uses. A join would need a
// scanner of its own, and two readings of one row's columns are how the two
// eventually disagree about what a column means.
//
// The keys are matched on (link_id, content_key) — the pair the explanation is
// unique by — and never on the mirror row's id, which the explanation
// deliberately does not name (see the migration).
func (s *Store) attachExplanations(ctx context.Context, rows []MirrorRow) error {
	if len(rows) == 0 {
		return nil
	}
	links := make([]uuid.UUID, len(rows))
	keys := make([]string, len(rows))
	for i, m := range rows {
		links[i] = m.LinkID
		keys[i] = m.ContentKey
	}

	// WITH ORDINALITY, so each answer comes back knowing WHICH of the pairs
	// asked for it. Rebuilding the key on this side to look the answer up in a
	// map is the fault this codebase has already paid for once: a value that
	// travels to the database and back is not always the value that was sent
	// (a date goes as a date and returns as midnight UTC), and the second
	// computation of the key is where the two part company.
	q := `SELECT p.ord, e.id, e.operation_id, o.occurred_on, o.type
		    FROM unnest($1::uuid[], $2::text[]) WITH ORDINALITY AS p(link_id, content_key, ord)
		    JOIN tinvest_mirror_explanations e
		      ON e.link_id = p.link_id AND e.content_key = p.content_key
		    JOIN operations o ON o.id = e.operation_id`
	res, err := s.db.Query(ctx, q, links, keys)
	if err != nil {
		return fmt.Errorf("tinvest: attach the explanations of mirror rows: %w", err)
	}
	defer res.Close()
	for res.Next() {
		var (
			ord int
			e   RowExplanation
		)
		if err := res.Scan(&ord, &e.ID, &e.OperationID, &e.OperationOn, &e.OperationType); err != nil {
			return fmt.Errorf("tinvest: attach the explanations of mirror rows: %w", err)
		}
		if ord < 1 || ord > len(rows) {
			return fmt.Errorf("tinvest: attach the explanations of mirror rows: row %d of %d", ord, len(rows))
		}
		rows[ord-1].ExplainedBy = &e
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("tinvest: attach the explanations of mirror rows: %w", err)
	}
	return nil
}

// MirrorRowsByKeys returns the rows of one link named by content key, in no
// particular order. It is what the service checks a request against: every key
// it was given must be one of this link's rows.
func (s *Store) MirrorRowsByKeys(ctx context.Context, linkID uuid.UUID, keys []string) ([]MirrorRow, error) {
	return s.listMirrorRows(ctx, "list mirror rows by content key",
		`SELECT `+mirrorCols+` FROM tinvest_operations_mirror
		 WHERE link_id = $1 AND content_key = ANY($2)`, linkID, keys)
}

// CreateExplanations records that one manual operation accounts for these
// mirror rows of this link.
//
// All of them or none: one statement inside a transaction, so a request that
// names a key already explained leaves nothing behind. The unique violation is
// translated into ErrRowAlreadyExplained rather than passed on, because the
// caller has already checked for that case and this is the race — two requests
// explaining one row at once — where the database is the only authority.
func (s *Store) CreateExplanations(ctx context.Context, linkID, operationID uuid.UUID, keys []string) error {
	if len(keys) == 0 {
		return fmt.Errorf("tinvest: create explanations: no content keys given")
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO tinvest_mirror_explanations (link_id, content_key, operation_id)
		SELECT $1, k, $2 FROM unnest($3::text[]) AS k`, linkID, operationID, keys)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return ErrRowAlreadyExplained
		}
		return fmt.Errorf("tinvest: create explanations: %w", err)
	}
	return nil
}

// uniqueViolation is PostgreSQL's own code for a broken unique constraint.
const uniqueViolation = "23505"

// ExplanationByID returns one explanation with the connection and space it
// belongs to, so a caller can check the space before acting on it.
func (s *Store) ExplanationByID(ctx context.Context, id uuid.UUID) (Explanation, error) {
	var e Explanation
	err := s.db.QueryRow(ctx, `
		SELECT e.id, e.link_id, l.connection_id, l.space_id, e.content_key, e.operation_id, e.created_at
		  FROM tinvest_mirror_explanations e
		  JOIN tinvest_account_links l ON l.id = e.link_id
		 WHERE e.id = $1`, id).
		Scan(&e.ID, &e.LinkID, &e.ConnectionID, &e.SpaceID, &e.ContentKey, &e.OperationID, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Explanation{}, ErrExplanationNotFound
	}
	if err != nil {
		return Explanation{}, fmt.Errorf("tinvest: read an explanation: %w", err)
	}
	return e, nil
}

// LinkByID returns one linked account of a space.
func (s *Store) LinkByID(ctx context.Context, spaceID, id uuid.UUID) (AccountLink, error) {
	link, err := scanLink(s.db.QueryRow(ctx,
		`SELECT `+linkCols+` FROM tinvest_account_links WHERE id = $1 AND space_id = $2`, id, spaceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountLink{}, ErrLinkNotFound
	}
	if err != nil {
		return AccountLink{}, fmt.Errorf("tinvest: read an account link: %w", err)
	}
	return link, nil
}

// ErrLinkNotFound is returned when no linked account of this space carries the
// id asked for.
var ErrLinkNotFound = errors.New("tinvest: account link not found")
