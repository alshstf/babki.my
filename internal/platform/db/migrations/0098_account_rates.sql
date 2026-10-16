-- +goose Up
-- A savings account's or a deposit's rate (decision Р-51 А), a row from the day
-- it holds: fixed, or the Central Bank's key rate on each day plus a margin, as
-- deposits tied to it are. by_hand: the family wrote it in, over anything a
-- catalog of the banks' programmes would say (Р-52).
CREATE TABLE account_rates (
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    space_id   uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    from_on    date NOT NULL,
    kind       text NOT NULL CHECK (kind IN ('fixed', 'key')),
    percent    numeric(6,3) NOT NULL,
    by_hand    boolean NOT NULL DEFAULT true,
    PRIMARY KEY (account_id, from_on),
    CHECK ((kind = 'fixed' AND percent >= 0 AND percent < 100) OR (kind = 'key' AND percent > -50 AND percent < 50))
);
CREATE INDEX account_rates_space_idx ON account_rates (space_id);

-- +goose Down
DROP TABLE account_rates;
