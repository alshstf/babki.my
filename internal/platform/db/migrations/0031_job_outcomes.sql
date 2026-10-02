-- +goose Up
-- How each kind of background job last ended: when it last succeeded, and when
-- and how it last failed. One row a kind, written after every attempt, so that
-- a source that stopped answering can be seen without reading the logs.
CREATE TABLE job_outcomes (
    kind            TEXT PRIMARY KEY,
    last_success_at TIMESTAMPTZ,
    last_failure_at TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT ''
);

-- +goose Down
DROP TABLE job_outcomes;
