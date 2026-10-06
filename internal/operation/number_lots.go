package operation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"babki.my/babki/internal/portfolio"
)

// legacyMove is a move between two of a family's accounts whose breakdown was
// recorded before lots had numbers.
type legacyMove struct {
	spaceID, outID, outAccount, inAccount, carrier uuid.UUID
}

// legacyMoves lists them, oldest first.
func (s *Store) legacyMoves(ctx context.Context) ([]legacyMove, error) {
	rows, err := s.db.Query(ctx, `
		SELECT o.space_id, o.id, o.account_id, i.account_id, i.id
		FROM operations o
		JOIN operations i ON i.space_id = o.space_id AND i.transfer_group_id = o.transfer_group_id
			AND i.type = 'transfer_in'
		WHERE o.type = 'transfer_out'
			AND EXISTS (SELECT 1 FROM operation_transfer_lots l WHERE l.operation_id = i.id AND l.from_origin IS NULL)
		ORDER BY o.occurred_on, o.created_at`)
	if err != nil {
		return nil, fmt.Errorf("list moves without lot numbers: %w", err)
	}
	defer rows.Close()
	var out []legacyMove
	for rows.Next() {
		var m legacyMove
		if err := rows.Scan(&m.spaceID, &m.outID, &m.outAccount, &m.inAccount, &m.carrier); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// setPiecesFrom writes the lot numbers of a stored breakdown, piece by piece.
func (s *Store) setPiecesFrom(ctx context.Context, carrier uuid.UUID, pieces []ReleasedLot) error {
	for seq, pc := range pieces {
		origin, from := lotFrom(pc.From)
		if _, err := s.db.Exec(ctx, `UPDATE operation_transfer_lots SET from_origin = $3, from_seq = $4
			WHERE operation_id = $1 AND seq = $2`, carrier, seq, origin, from); err != nil {
			return err
		}
	}
	return nil
}

// errLeftAsItWas rolls one move's numbering back.
var errLeftAsItWas = errors.New("left to the day matching")

// NumberLegacyMoves gives the pieces of every move recorded before lots had
// numbers the number of the lot each came from (plan 2.1), where the day
// matching names exactly one, and only when both accounts still replay
// afterwards. It reports how many moves it numbered and how many it left to
// the day matching. Imported and registry breakdowns are renumbered by their
// own writers on their next run.
func (s *Service) NumberLegacyMoves(ctx context.Context) (numbered, left int, err error) {
	moves, err := s.store.legacyMoves(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, m := range moves {
		err := s.store.WithAccountsLocked(ctx, m.spaceID, []uuid.UUID{m.outAccount, m.inAccount}, func(st *Store) error {
			return numberMove(ctx, st, m)
		})
		switch {
		case err == nil:
			numbered++
		case errors.Is(err, errLeftAsItWas):
			left++
		default:
			return numbered, left, err
		}
	}
	return numbered, left, nil
}

func numberMove(ctx context.Context, st *Store, m legacyMove) error {
	journal, err := st.ListForEngine(ctx, m.spaceID, m.outAccount)
	if err != nil {
		return err
	}
	var out Operation
	for _, o := range journal {
		if o.ID == m.outID {
			out = o
		}
	}
	if out.ID != m.outID || out.InstrumentID == nil {
		return errLeftAsItWas
	}
	before, err := portfolio.Compute(foldedAhead(journal, out))
	if err != nil {
		return errLeftAsItWas
	}
	p, ok := before[*out.InstrumentID]
	if !ok {
		return errLeftAsItWas
	}
	pieces, ok := portfolio.NumberLegacyPieces(p.Lots, out.TransferLots)
	if !ok {
		return errLeftAsItWas
	}
	if err := st.setPiecesFrom(ctx, m.carrier, pieces); err != nil {
		return err
	}
	for _, account := range []uuid.UUID{m.outAccount, m.inAccount} {
		after, err := st.ListForEngine(ctx, m.spaceID, account)
		if err != nil {
			return err
		}
		if _, err := portfolio.Compute(after); err != nil {
			return errLeftAsItWas
		}
	}
	return nil
}

// NumberLotsArgs is the job that numbers the lots of moves recorded before
// lots had numbers. It runs at start and daily; with nothing left it is one
// query.
type NumberLotsArgs struct{}

func (NumberLotsArgs) Kind() string { return "operation.number_lots" }

type numberLotsWorker struct {
	river.WorkerDefaults[NumberLotsArgs]
	svc *Service
	log *slog.Logger
}

// NewNumberLotsWorker builds the job's worker.
func NewNumberLotsWorker(svc *Service, log *slog.Logger) river.Worker[NumberLotsArgs] {
	return &numberLotsWorker{svc: svc, log: log}
}

func (w *numberLotsWorker) Timeout(*river.Job[NumberLotsArgs]) time.Duration { return 10 * time.Minute }

func (w *numberLotsWorker) Work(ctx context.Context, _ *river.Job[NumberLotsArgs]) error {
	numbered, left, err := w.svc.NumberLegacyMoves(ctx)
	if numbered > 0 || left > 0 {
		w.log.Info("operation: numbered the lots of moves recorded before lots had numbers",
			"numbered", numbered, "left_to_day_matching", left)
	}
	return err
}
