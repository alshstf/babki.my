package jobs

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/rivertype"
)

// NewOutcomeHook exposes the hook that records how jobs end.
func NewOutcomeHook(pool *pgxpool.Pool, log *slog.Logger) rivertype.HookWorkEnd {
	return &outcomeHook{pool: pool, log: log}
}
