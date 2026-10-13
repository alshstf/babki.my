-- +goose Up
-- Savings goals (decision Р-44): a sum by a date with a contribution a month,
-- each optional, the family's expected yield a year on it, and the money that
-- stands for it — whole accounts or a sum out of one.
CREATE TABLE savings_goals (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id      uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    name          text NOT NULL CHECK (name <> ''),
    target_minor  bigint CHECK (target_minor > 0),
    target_on     date,
    monthly_minor bigint CHECK (monthly_minor > 0),
    yield_percent numeric(5,2) NOT NULL DEFAULT 0 CHECK (yield_percent >= 0 AND yield_percent < 100),
    position      integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX savings_goals_space_idx ON savings_goals (space_id);

CREATE TABLE savings_goal_funds (
    goal_id      uuid NOT NULL REFERENCES savings_goals(id) ON DELETE CASCADE,
    account_id   uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    amount_minor bigint CHECK (amount_minor > 0),
    PRIMARY KEY (goal_id, account_id)
);

-- +goose Down
DROP TABLE savings_goal_funds;
DROP TABLE savings_goals;
