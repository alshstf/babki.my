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

// ErrRowNotInLink: a named content key is not one of the link's mirror rows.
// A 404.
var ErrRowNotInLink = errors.New("tinvest: no mirror row of this link carries that content key")

// ErrRowAlreadyExplained: a named mirror row already has an explanation.
// A 409.
var ErrRowAlreadyExplained = errors.New("tinvest: this mirror row is already accounted for by hand")

// ErrExplanationNotFound is returned when no explanation of this space carries
// the id asked for.
var ErrExplanationNotFound = errors.New("tinvest: explanation not found")

// RowExplanation is the manual operation accounting for one mirror row, as a
// listing shows it: date and type to find it in the journal, not a copy of its
// figures.
type RowExplanation struct {
	ID          uuid.UUID
	OperationID uuid.UUID
	// OperationOn is the operation's journal date, which need not match either
	// explained row (a fund redemption's two halves are a fortnight apart).
	OperationOn   time.Time
	OperationType string
}

// Explanation is one explanations row with its connection, for the caller's
// authorization.
type Explanation struct {
	ID           uuid.UUID
	LinkID       uuid.UUID
	ConnectionID uuid.UUID
	SpaceID      uuid.UUID
	ContentKey   string
	OperationID  uuid.UUID
	CreatedAt    time.Time
}

// ExplainedKeysByLink is the set of the link's mirror rows explained by hand,
// by content key.
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

// attachExplanations fills ExplainedBy. A second query, not a join, so mirror
// columns are read by the one scanner every mirror query uses. Matched on
// (link_id, content_key), never the mirror row id.
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

	// WITH ORDINALITY: each answer knows which pair asked for it, rather than
	// rebuilding the key from values that come back changed (a date returns as
	// midnight UTC).
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

// MirrorRowsByKeys returns the link's rows with these content keys, in no
// order.
func (s *Store) MirrorRowsByKeys(ctx context.Context, linkID uuid.UUID, keys []string) ([]MirrorRow, error) {
	return s.listMirrorRows(ctx, "list mirror rows by content key",
		`SELECT `+mirrorCols+` FROM tinvest_operations_mirror
		 WHERE link_id = $1 AND content_key = ANY($2)`, linkID, keys)
}

// CreateExplanations records, all or none, that one manual operation accounts
// for these mirror rows. A unique violation (two requests racing) becomes
// ErrRowAlreadyExplained.
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
