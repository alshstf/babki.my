-- +goose Up
-- A loan's terms (household stage 2, decision Р-24: «кредиты и ипотека с
-- графиком»): what was borrowed, at what yearly rate, for how many months,
-- from when, and how it is repaid. The schedule is worked out from them; the
-- journal stays the record of what was actually paid.
CREATE TABLE loans (
    account_id      uuid PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    space_id        uuid NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    principal_minor bigint NOT NULL CHECK (principal_minor > 0),
    annual_rate     numeric(9,4) NOT NULL CHECK (annual_rate >= 0 AND annual_rate < 1000),
    term_months     integer NOT NULL CHECK (term_months BETWEEN 1 AND 600),
    issued_on       date NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('annuity', 'differentiated')),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE loans;
