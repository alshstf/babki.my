-- +goose Up
-- What an earning category means for tax (decision Р-41): wages — the main
-- base, paid net of the employer's withholding — or deposits' interest. The
-- default set's, kept under their names, get theirs once.
ALTER TABLE categories ADD COLUMN tax_mark text
    CHECK (tax_mark IS NULL OR (tax_mark IN ('wages', 'interest') AND kind = 'income'));
UPDATE categories SET tax_mark = 'wages'
    WHERE kind = 'income' AND parent_id IS NULL AND name IN ('Зарплата', 'Премии');
UPDATE categories SET tax_mark = 'interest'
    WHERE kind = 'income' AND parent_id IS NULL AND name = 'Проценты по вкладам';

-- +goose Down
ALTER TABLE categories DROP COLUMN tax_mark;
