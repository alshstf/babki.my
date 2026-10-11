package mailbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/platform/db"
	"babki.my/babki/internal/platform/secretbox"
	"babki.my/babki/internal/receipt"
)

// ErrNotFound is a space with no mailbox: a 404.
var ErrNotFound = fmt.Errorf("mailbox: %w", pgx.ErrNoRows)

// Service keeps the spaces' mailboxes and reads them.
type Service struct {
	db       db.Executor
	box      *secretbox.Box
	reader   Reader
	receipts importer
	log      *slog.Logger
	now      func() time.Time
}

// NewService keeps the boxes; box seals the app passwords — nil when the
// program runs without an encryption key, and then no box can be stated.
func NewService(x db.Executor, box *secretbox.Box, reader Reader, receipts importer, log *slog.Logger) *Service {
	return &Service{db: x, box: box, reader: reader, receipts: receipts, log: log, now: time.Now}
}

// Get is the space's mailbox, without its password.
func (s *Service) Get(ctx context.Context, spaceID uuid.UUID) (Box, error) {
	var b Box
	var problem string
	err := s.db.QueryRow(ctx, `SELECT host, port, username, folder, checked_at, last_error, last_found
		FROM mailboxes WHERE space_id = $1`, spaceID).
		Scan(&b.Host, &b.Port, &b.Username, &b.Folder, &b.CheckedAt, &problem, &b.LastFound)
	if errors.Is(err, pgx.ErrNoRows) {
		return Box{}, ErrNotFound
	}
	if err != nil {
		return Box{}, fmt.Errorf("mailbox: get: %w", err)
	}
	b.Problem = Problem(problem)
	return b, nil
}

// Set states the space's mailbox. A new box needs its password; a box
// stated again keeps its password when none is given, and is read anew from
// its first letter when it is another box.
func (s *Service) Set(ctx context.Context, spaceID uuid.UUID, in Settings) (Box, error) {
	in, err := in.clean()
	if err != nil {
		return Box{}, err
	}
	if s.box == nil {
		return Box{}, fmt.Errorf("%w: the program runs without an encryption key (BABKI_ENCRYPTION_KEY), so it cannot keep a password", family.ErrValidation)
	}
	var sealed []byte
	if in.Password != nil {
		sealed = s.box.Seal([]byte(*in.Password))
	}
	_, err = s.Get(ctx, spaceID)
	switch {
	case errors.Is(err, ErrNotFound) && sealed == nil:
		return Box{}, fmt.Errorf("%w: a new mailbox needs its app password", family.ErrValidation)
	case errors.Is(err, ErrNotFound):
		_, err = s.db.Exec(ctx, `INSERT INTO mailboxes (space_id, host, port, username, folder, password_sealed)
			VALUES ($1, $2, $3, $4, $5, $6)`, spaceID, in.Host, in.Port, in.Username, in.Folder, sealed)
	case err == nil:
		_, err = s.db.Exec(ctx, `UPDATE mailboxes SET host = $2, port = $3, username = $4, folder = $5,
				password_sealed = COALESCE($6, password_sealed),
				uid_validity = CASE WHEN (host, username, folder) = ($2, $4, $5) THEN uid_validity ELSE 0 END,
				last_uid = CASE WHEN (host, username, folder) = ($2, $4, $5) THEN last_uid ELSE 0 END,
				last_error = ''
			WHERE space_id = $1`, spaceID, in.Host, in.Port, in.Username, in.Folder, sealed)
	}
	if err != nil {
		return Box{}, fmt.Errorf("mailbox: set: %w", err)
	}
	return s.Get(ctx, spaceID)
}

// Delete forgets the space's mailbox and its password.
func (s *Service) Delete(ctx context.Context, spaceID uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM mailboxes WHERE space_id = $1`, spaceID)
	if err != nil {
		return fmt.Errorf("mailbox: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Check reads the letters that came since the last reading and takes the
// receipts they hold. A failure to reach or read the box is noted on it for
// the family to see, and is no error of the call.
func (s *Service) Check(ctx context.Context, spaceID uuid.UUID) (Box, receipt.ImportResult, error) {
	var set Settings
	var sealed []byte
	var validity, last int64
	err := s.db.QueryRow(ctx, `SELECT host, port, username, folder, password_sealed, uid_validity, last_uid
		FROM mailboxes WHERE space_id = $1`, spaceID).
		Scan(&set.Host, &set.Port, &set.Username, &set.Folder, &sealed, &validity, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return Box{}, receipt.ImportResult{}, ErrNotFound
	}
	if err != nil {
		return Box{}, receipt.ImportResult{}, fmt.Errorf("mailbox: check: %w", err)
	}
	if s.box == nil {
		return Box{}, receipt.ImportResult{}, fmt.Errorf("mailbox: the program runs without an encryption key")
	}
	password, err := s.box.Open(sealed)
	if err != nil {
		// Sealed with a key the program no longer has (a key change not
		// finished with reseal): the family states the password again.
		s.log.Warn("mailbox: the app password cannot be opened with this encryption key", "space", spaceID, "error", err)
		if _, err := s.db.Exec(ctx, `UPDATE mailboxes SET checked_at = $2, last_error = $3 WHERE space_id = $1`,
			spaceID, s.now(), ProblemKey); err != nil {
			return Box{}, receipt.ImportResult{}, fmt.Errorf("mailbox: note: %w", err)
		}
		b, err := s.Get(ctx, spaceID)
		return b, receipt.ImportResult{}, err
	}
	newValidity, letters, readErr := s.reader.Read(ctx, set, string(password), uint32(last), uint32(validity))
	if readErr != nil {
		problem := ProblemRead
		var re readError
		if errors.As(readErr, &re) {
			problem = re.problem
		}
		s.log.Warn("mailbox: reading failed", "space", spaceID, "problem", problem, "error", readErr)
		if _, err := s.db.Exec(ctx, `UPDATE mailboxes SET checked_at = $2, last_error = $3 WHERE space_id = $1`,
			spaceID, s.now(), problem); err != nil {
			return Box{}, receipt.ImportResult{}, fmt.Errorf("mailbox: note: %w", err)
		}
		b, err := s.Get(ctx, spaceID)
		return b, receipt.ImportResult{}, err
	}
	var found []receipt.Receipt
	newLast := uint32(last)
	if newValidity != uint32(validity) {
		newLast = 0
	}
	for _, l := range letters {
		rs, err := receipt.ParseMail(l.Raw)
		if err != nil {
			s.log.Warn("mailbox: a letter could not be read", "space", spaceID, "uid", l.UID, "error", err)
		}
		found = append(found, rs...)
		newLast = max(newLast, l.UID)
	}
	res, err := s.receipts.Import(ctx, spaceID, found)
	if err != nil {
		return Box{}, receipt.ImportResult{}, err
	}
	if _, err := s.db.Exec(ctx, `UPDATE mailboxes SET checked_at = $2, last_error = '', last_found = $3,
		uid_validity = $4, last_uid = $5 WHERE space_id = $1`,
		spaceID, s.now(), res.Found, int64(newValidity), int64(newLast)); err != nil {
		return Box{}, receipt.ImportResult{}, fmt.Errorf("mailbox: note: %w", err)
	}
	b, err := s.Get(ctx, spaceID)
	return b, res, err
}

// Reseal re-encrypts every app password with the current key so an old key
// can be dropped — part of `babki reseal`, with the brokers' tokens; all or
// none. Returns how many.
func (s *Service) Reseal(ctx context.Context) (int, error) {
	if s.box == nil {
		return 0, fmt.Errorf("mailbox: the program runs without an encryption key")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT space_id, password_sealed FROM mailboxes FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("mailbox: read passwords: %w", err)
	}
	type sealed struct {
		space    uuid.UUID
		password []byte
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (sealed, error) {
		var c sealed
		return c, r.Scan(&c.space, &c.password)
	})
	if err != nil {
		return 0, err
	}
	for _, c := range all {
		plain, err := s.box.Open(c.password)
		if err != nil {
			return 0, fmt.Errorf("mailbox: space %s: no key opens its app password: %w", c.space, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE mailboxes SET password_sealed = $2 WHERE space_id = $1`,
			c.space, s.box.Seal(plain)); err != nil {
			return 0, fmt.Errorf("mailbox: reseal space %s: %w", c.space, err)
		}
	}
	return len(all), tx.Commit(ctx)
}

// Spaces are the spaces with a mailbox, for the hourly reading.
func (s *Service) Spaces(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx, `SELECT space_id FROM mailboxes ORDER BY space_id`)
	if err != nil {
		return nil, fmt.Errorf("mailbox: spaces: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}
