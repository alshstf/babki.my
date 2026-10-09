-- +goose Up
-- What each running background job the screen shows is doing: a row while the
-- job runs, deleted when it ends (internal/platform/jobs, progress.go). The
-- queue writes it and keeps updated_at fresh; a row whose job died with its
-- process stops being shown once updated_at is old. An empty space_id is a job
-- of the whole instance, which every space sees; account_ids are the accounts
-- whose figures are not final until it ends.
CREATE TABLE job_progress (
    job_id      bigint PRIMARY KEY,
    kind        text NOT NULL,
    space_id    uuid,
    account_ids uuid[] NOT NULL DEFAULT '{}',
    stage       text NOT NULL DEFAULT '',
    done        integer NOT NULL DEFAULT 0 CHECK (done >= 0),
    total       integer NOT NULL DEFAULT 0 CHECK (total >= 0),
    started_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE job_progress;
