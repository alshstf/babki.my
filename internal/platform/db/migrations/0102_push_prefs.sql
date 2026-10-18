-- +goose Up
-- What a member's reminders tell and when they keep quiet (decision Р-61 Б):
-- the kinds of reminder turned off, and the hours from quiet_from to quiet_to
-- by the server's clock when nothing is sent — the night by default, as
-- before. Equal hours: never quiet. No row: everything, quiet at night.
CREATE TABLE push_prefs (
    user_id    uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    off        text[] NOT NULL DEFAULT '{}',
    quiet_from smallint NOT NULL DEFAULT 21 CHECK (quiet_from BETWEEN 0 AND 23),
    quiet_to   smallint NOT NULL DEFAULT 9 CHECK (quiet_to BETWEEN 0 AND 23)
);

-- +goose Down
DROP TABLE push_prefs;
