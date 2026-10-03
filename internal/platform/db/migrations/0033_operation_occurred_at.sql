-- +goose Up
-- The instant the source says an operation happened, when it says one. Within a
-- day the journal folds rows by it, ahead of the rows without one, instead of by
-- the moment each row reached the journal (#198).
--
-- Not filled in here for rows already imported: the importer is the one writer
-- of its rows, and its next rebuild sees the instant missing and rewrites them,
-- each keeping its place (see migration 0026 for the same rule).
ALTER TABLE operations ADD COLUMN occurred_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE operations DROP COLUMN occurred_at;
