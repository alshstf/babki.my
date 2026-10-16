-- +goose Up
-- A savings account's or a deposit's programme of its bank in the program's
-- catalog (decisions Р-52 Б, Р-53 Б): the variant chosen at opening, the day
-- it was opened (or last prolonged), a deposit's term in the bank's unit and
-- the sum it was opened with. Its rate is reckoned from the catalog unless a
-- rate is written in by hand (account_rates), which stands over it.
CREATE TABLE account_programmes (
    account_id   uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    space_id     uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    programme    text NOT NULL,
    variant      text NOT NULL,
    opened_on    date NOT NULL,
    term         integer CHECK (term IS NULL OR term > 0),
    amount_minor bigint CHECK (amount_minor IS NULL OR amount_minor >= 0),
    conditions   text NOT NULL DEFAULT 'manual' CHECK (conditions IN ('manual', 'auto')),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX account_programmes_space_idx ON account_programmes (space_id);

-- The family's marks of a programme's conditions: from from_on on the
-- condition holds at level (a bonus's step), or — met false — no longer
-- holds. A month counts the last mark made in it or before.
CREATE TABLE account_condition_marks (
    account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    space_id   uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    condition  text NOT NULL,
    from_on    date NOT NULL,
    level      integer NOT NULL DEFAULT 0 CHECK (level >= 0),
    met        boolean NOT NULL DEFAULT true,
    PRIMARY KEY (account_id, condition, from_on)
);
CREATE INDEX account_condition_marks_space_idx ON account_condition_marks (space_id);

-- +goose Down
DROP TABLE account_condition_marks;
DROP TABLE account_programmes;
