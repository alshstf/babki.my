-- +goose Up
-- Web push (decision Р-27): the devices a member turned reminders on for, and
-- the reminders already sent to each member, so each comes once. A device is
-- named by the endpoint its browser's push service gave it; the two keys
-- encrypt what is sent for that device alone.
CREATE TABLE push_subscriptions (
    endpoint   text PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    space_id   uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    p256dh     text NOT NULL,
    auth       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX push_subscriptions_space ON push_subscriptions (space_id);

CREATE TABLE push_sent (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key     text NOT NULL,
    sent_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

-- +goose Down
DROP TABLE push_sent;
DROP TABLE push_subscriptions;
