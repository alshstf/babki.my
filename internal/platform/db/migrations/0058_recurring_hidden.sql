-- +goose Up
-- Regular payments the family said are not regular (#428): three trips to
-- one shop a month apart look like a subscription. A payee is named as the
-- list folds it — case aside, «ё» as «е» — with its direction and currency.
CREATE TABLE recurring_hidden (
    space_id  uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    payee     text NOT NULL CHECK (length(payee) BETWEEN 1 AND 200),
    incoming  boolean NOT NULL,
    currency  text NOT NULL,
    hidden_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (space_id, payee, incoming, currency)
);

-- +goose Down
DROP TABLE recurring_hidden;
