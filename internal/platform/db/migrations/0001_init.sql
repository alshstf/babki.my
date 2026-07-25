-- +goose Up
CREATE TABLE meta (
    key        TEXT PRIMARY KEY,
    value      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO meta (key, value) VALUES ('instance_id', gen_random_uuid()::text);

-- +goose Down
DROP TABLE meta;
