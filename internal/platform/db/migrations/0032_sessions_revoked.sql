-- +goose Up
-- The moment a user's sessions signed in before it stopped counting: set when
-- the password changes or the user signs out everywhere else.
ALTER TABLE users ADD COLUMN sessions_revoked_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE users DROP COLUMN sessions_revoked_at;
