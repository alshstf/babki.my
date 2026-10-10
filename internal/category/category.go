// Package category keeps a family's categories of money going out and coming
// in (household stage 1, decision Р-24): a tree two levels deep, each a
// spending or an earning one. A spending is a withdrawal with a spending
// category, an earning a deposit with an earning one; the journal stays the
// one record of money, and the categories only say what it was for.
package category

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"babki.my/babki/internal/family"
)

// Kind says whether a category is money going out or coming in.
type Kind string

const (
	KindExpense Kind = "expense"
	KindIncome  Kind = "income"
)

func (k Kind) Valid() bool { return k == KindExpense || k == KindIncome }

// Category is one category of a family's.
type Category struct {
	ID       uuid.UUID
	SpaceID  uuid.UUID
	ParentID *uuid.UUID
	Kind     Kind
	Name     string
	// Archived keeps a category its operations still name, out of the lists
	// a new entry picks from.
	Archived  bool
	Position  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MaxNameRunes bounds a name, as the schema's CHECK does.
const MaxNameRunes = 60

var (
	// ErrNotFound is a category that is not the space's: a 404 (WriteError
	// answers pgx.ErrNoRows so).
	ErrNotFound = fmt.Errorf("category: %w", pgx.ErrNoRows)
	// ErrNameTaken is a second category of one name on one level of a kind.
	ErrNameTaken = fmt.Errorf("%w: a category of this name already sits at this place", family.ErrValidation)
	// ErrHasChildren refuses removing a category others sit under.
	ErrHasChildren = fmt.Errorf("%w: the category has categories under it; move or remove them first", family.ErrValidation)
	// ErrInUse refuses removing a category an operation names: archive it.
	ErrInUse = fmt.Errorf("%w: operations name this category; archive it instead", family.ErrValidation)
)

// cleanName trims a name and checks its length.
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n == 0 || n > MaxNameRunes {
		return "", fmt.Errorf("%w: a category's name is 1 to %d characters", family.ErrValidation, MaxNameRunes)
	}
	return name, nil
}

// seed is one category of the default set, with the ones under it.
type seed struct {
	name     string
	children []string
}

// defaults is the set a family starts with: what Russian household apps and
// banks commonly split spending into, kept short so it is edited, not read.
var defaults = map[Kind][]seed{
	KindExpense: {
		{"Продукты", nil},
		{"Кафе и рестораны", nil},
		{"Транспорт", []string{"Такси", "Общественный транспорт"}},
		{"Автомобиль", []string{"Топливо", "Обслуживание и ремонт", "Парковка и штрафы"}},
		{"Дом", []string{"Аренда", "Коммунальные платежи", "Ремонт и мебель"}},
		{"Связь и интернет", nil},
		{"Здоровье", []string{"Аптеки", "Врачи и анализы"}},
		{"Одежда и обувь", nil},
		{"Дети", nil},
		{"Образование", nil},
		{"Развлечения и хобби", nil},
		{"Путешествия", nil},
		{"Подарки", nil},
		{"Красота и уход", nil},
		{"Подписки и сервисы", nil},
		{"Домашние животные", nil},
		{"Налоги и сборы", nil},
		{"Проценты по кредитам", nil},
		{"Банковские комиссии", nil},
		{"Прочие расходы", nil},
	},
	KindIncome: {
		{"Зарплата", nil},
		{"Премии", nil},
		{"Подработка", nil},
		{"Проценты по вкладам", nil},
		{"Кэшбэк", nil},
		{"Подарки", nil},
		{"Возврат долга", nil},
		{"Продажа вещей", nil},
		{"Прочие доходы", nil},
	},
}
