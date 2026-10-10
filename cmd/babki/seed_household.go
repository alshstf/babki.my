package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/category"
	"babki.my/babki/internal/operation"
)

// seedHousehold gives the demo family two months of everyday money (household
// stage 1): salaries in, rent, groceries, a taxi out, each under a default
// category and with whom it was, on the card, the current account and the
// cash. One spending is left unfiled, so the journal's «без категории» has
// something to find.
func seedHousehold(ctx context.Context, tx pgx.Tx, spaceID uuid.UUID,
	accIDs map[string]uuid.UUID, d func(string) time.Time,
) error {
	categories, err := category.NewStore(tx).List(ctx, spaceID)
	if err != nil {
		return fmt.Errorf("seed household: categories: %w", err)
	}
	byName := map[string]uuid.UUID{}
	for _, c := range categories {
		byName[string(c.Kind)+"/"+c.Name] = c.ID
	}

	type entry struct {
		account, date string
		typ           operation.Type
		rub           int64 // whole roubles, positive; the sign follows the type
		category      string
		counterparty  string
	}
	in, out := operation.TypeDeposit, operation.TypeWithdrawal
	entries := []entry{
		{"Текущий Сбер", "2026-08-05", in, 180_000, "income/Зарплата", "ООО «Ромашка»"},
		{"Текущий Сбер", "2026-08-06", out, 45_000, "expense/Аренда", "ИП Смирнова"},
		{"Текущий Сбер", "2026-08-10", out, 6_200, "expense/Коммунальные платежи", "ЕИРЦ"},
		{"Текущий Сбер", "2026-08-12", out, 890, "expense/Связь и интернет", "МТС"},
		{"Текущий Сбер", "2026-08-15", out, 3_500, "expense/Врачи и анализы", "Инвитро"},
		{"Текущий Сбер", "2026-08-25", in, 60_000, "income/Зарплата", "ООО «Ромашка»"},
		{"Текущий Сбер", "2026-08-28", out, 12_000, "expense/Образование", "Skyeng"},
		{"Текущий Сбер", "2026-09-05", in, 180_000, "income/Зарплата", "ООО «Ромашка»"},
		{"Текущий Сбер", "2026-09-06", out, 45_000, "expense/Аренда", "ИП Смирнова"},
		{"Текущий Сбер", "2026-09-10", out, 7_100, "expense/Коммунальные платежи", "ЕИРЦ"},
		{"Текущий Сбер", "2026-09-12", out, 890, "expense/Связь и интернет", "МТС"},
		{"Текущий Сбер", "2026-09-18", in, 1_850, "income/Кэшбэк", "Сбер"},
		{"Текущий Сбер", "2026-09-25", in, 60_000, "income/Зарплата", "ООО «Ромашка»"},
		{"Текущий Сбер", "2026-09-27", out, 15_000, "", "Иван П."},
		{"Текущий Сбер", "2026-09-30", operation.TypeFee, 99, "expense/Банковские комиссии", "Сбер"},

		{"Кредитка Альфа", "2026-08-03", out, 4_300, "expense/Продукты", "Пятёрочка"},
		{"Кредитка Альфа", "2026-08-09", out, 2_150, "expense/Кафе и рестораны", "Шоколадница"},
		{"Кредитка Альфа", "2026-08-11", out, 640, "expense/Такси", "Яндекс Go"},
		{"Кредитка Альфа", "2026-08-14", out, 3_200, "expense/Топливо", "Лукойл"},
		{"Кредитка Альфа", "2026-08-17", out, 5_800, "expense/Продукты", "Перекрёсток"},
		{"Кредитка Альфа", "2026-08-22", out, 7_990, "expense/Одежда и обувь", "Lamoda"},
		{"Кредитка Альфа", "2026-08-24", out, 1_200, "expense/Аптеки", "Ригла"},
		{"Кредитка Альфа", "2026-08-30", out, 399, "expense/Подписки и сервисы", "Яндекс Плюс"},
		{"Кредитка Альфа", "2026-09-02", out, 4_900, "expense/Продукты", "Пятёрочка"},
		{"Кредитка Альфа", "2026-09-07", out, 3_100, "expense/Кафе и рестораны", "Теремок"},
		{"Кредитка Альфа", "2026-09-08", out, 18_500, "expense/Путешествия", "РЖД"},
		{"Кредитка Альфа", "2026-09-13", out, 3_400, "expense/Топливо", "Лукойл"},
		{"Кредитка Альфа", "2026-09-16", out, 6_100, "expense/Продукты", "ВкусВилл"},
		{"Кредитка Альфа", "2026-09-19", out, 2_500, "expense/Развлечения и хобби", "Каро"},
		{"Кредитка Альфа", "2026-09-21", out, 780, "expense/Такси", "Яндекс Go"},
		{"Кредитка Альфа", "2026-09-26", out, 3_300, "expense/Подарки", "Золотое яблоко"},
		{"Кредитка Альфа", "2026-09-30", out, 399, "expense/Подписки и сервисы", "Яндекс Плюс"},

		{"Наличные", "2026-08-18", out, 1_500, "expense/Продукты", "Рынок"},
		{"Наличные", "2026-09-14", out, 2_000, "expense/Дети", "Кружок рисования"},
		{"Наличные", "2026-09-20", in, 5_000, "income/Подарки", "Бабушка"},
	}

	opSvc := operation.NewService(operation.NewStore(tx))
	for _, e := range entries {
		amount := e.rub * 100
		if e.typ != in {
			amount = -amount
		}
		op := operation.Operation{
			AccountID: accIDs[e.account], Type: e.typ, OccurredOn: d(e.date),
			AmountMinor: amount, Currency: "RUB", Counterparty: e.counterparty,
		}
		if e.category != "" {
			id, ok := byName[e.category]
			if !ok {
				return fmt.Errorf("seed household: no default category %q", e.category)
			}
			op.CategoryID = &id
		}
		if _, err := opSvc.Create(ctx, spaceID, op); err != nil {
			return fmt.Errorf("seed household %s %s %s: %w", e.account, e.date, e.category, err)
		}
	}
	return nil
}
