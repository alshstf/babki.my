package table

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"babki.my/babki/internal/operation"
	"babki.my/babki/internal/platform/db"
)

// Import is one load of a table into an account.
type Import struct {
	ID            uuid.UUID
	AccountID     uuid.UUID
	FileName      string
	Mapping       Mapping
	Written       int
	Duplicate     int
	Unparsed      int
	Refused       int
	CreatedAt     time.Time
	RolledBackAt  *time.Time
	OperationsNow int // how many of the operations it wrote are still in the journal
}

// Store keeps the record of table imports.
type Store struct{ db db.Executor }

func NewStore(x db.Executor) *Store { return &Store{db: x} }

type storedMapping struct {
	HasHeader bool                      `json:"has_header"`
	Columns   map[Field]int             `json:"columns"`
	Types     map[string]operation.Type `json:"types"`
}

const importCols = `i.id, i.account_id, i.file_name, i.mapping, i.rows_written, i.rows_duplicate,
	i.rows_unparsed, i.rows_refused, i.created_at, i.rolled_back_at,
	(SELECT count(*) FROM table_import_operations o WHERE o.import_id = i.id)`

func scanImport(row interface{ Scan(...any) error }) (Import, error) {
	var (
		imp Import
		raw []byte
	)
	if err := row.Scan(&imp.ID, &imp.AccountID, &imp.FileName, &raw, &imp.Written, &imp.Duplicate,
		&imp.Unparsed, &imp.Refused, &imp.CreatedAt, &imp.RolledBackAt, &imp.OperationsNow); err != nil {
		return Import{}, err
	}
	var m storedMapping
	if err := json.Unmarshal(raw, &m); err != nil {
		return Import{}, err
	}
	imp.Mapping = Mapping(m)
	return imp, nil
}

// record writes a load and the operations it wrote, through q — the delta's
// own transaction.
func record(ctx context.Context, q db.Executor, spaceID, userID uuid.UUID, imp Import, ops []operation.Operation) (uuid.UUID, error) {
	raw, err := json.Marshal(storedMapping(imp.Mapping))
	if err != nil {
		return uuid.Nil, err
	}
	var createdBy *uuid.UUID
	if userID != uuid.Nil {
		createdBy = &userID
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO table_imports (space_id, account_id, file_name, mapping, rows_written,
			rows_duplicate, rows_unparsed, rows_refused, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		spaceID, imp.AccountID, imp.FileName, raw, imp.Written, imp.Duplicate, imp.Unparsed,
		imp.Refused, createdBy).Scan(&id); err != nil {
		return uuid.Nil, err
	}
	for _, op := range ops {
		if _, err := q.Exec(ctx, `INSERT INTO table_import_operations (import_id, operation_id) VALUES ($1, $2)`,
			id, op.ID); err != nil {
			return uuid.Nil, err
		}
	}
	return id, nil
}

// ByID reads one load in spaceID.
func (s *Store) ByID(ctx context.Context, spaceID, id uuid.UUID) (Import, error) {
	return scanImport(s.db.QueryRow(ctx, `SELECT `+importCols+` FROM table_imports i
		WHERE i.space_id = $1 AND i.id = $2`, spaceID, id))
}

// List reads an account's loads, newest first.
func (s *Store) List(ctx context.Context, spaceID, accountID uuid.UUID) ([]Import, error) {
	rows, err := s.db.Query(ctx, `SELECT `+importCols+` FROM table_imports i
		WHERE i.space_id = $1 AND i.account_id = $2 ORDER BY i.created_at DESC`, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Import{}
	for rows.Next() {
		imp, err := scanImport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, imp)
	}
	return out, rows.Err()
}

// operations lists the operations a load wrote that are still in the journal.
func (s *Store) operations(ctx context.Context, id uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `SELECT operation_id FROM table_import_operations WHERE import_id = $1`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var opID uuid.UUID
		if err := rows.Scan(&opID); err != nil {
			return nil, err
		}
		out = append(out, opID)
	}
	return out, rows.Err()
}

func markRolledBack(ctx context.Context, q db.Executor, id uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE table_imports SET rolled_back_at = now() WHERE id = $1`, id)
	return err
}
