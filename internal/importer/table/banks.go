package table

import "strings"

// TrackerTBank is Т-Банк's statement of operations — «Операции» in its web
// version, as CSV or Excel: every chosen account of the person in one file.
const TrackerTBank Tracker = "tbank"

// statementHeader is what a bank's statement is rearranged into, headed with
// words Guess knows. It has no type column: the amount's sign says.
var statementHeader = []string{"Дата", "Сумма", "Валюта", "Категория", "Контрагент", "Примечание", "MCC", "Счёт"}

const (
	stDate = iota
	stAmount
	stCurrency
	stCategory
	stCounterparty
	stNote
	stMCC
	stAccount
)

// isTBank is Т-Банк's statement by its own columns.
func isTBank(h headerIndex) bool {
	return h.has("датаоперации", "суммаввалютесчёта", "валютасчёта", "статус", "описание", "имясчёта")
}

// fromTBank rearranges Т-Банк's statement. The money is the account's — «Сумма
// в валюте счёта», not the operation's: a purchase in dollars on a ruble card
// took rubles off it, and the dollars stay in the note. The person's own
// category («Ваша категория») is over the bank's; the shop or the person paid
// is the counterparty, the transfer's message the note. An operation that did
// not go through («Ошибка») is shown and not loaded.
func fromTBank(t Table, h headerIndex) Table {
	out := rearranged(TrackerTBank, nil, t.Rows[0].Number, statementHeader)
	for _, line := range t.Rows[1:] {
		row := make([]string, len(statementHeader))
		row[stDate] = h.get(line, "датаоперации")
		row[stAmount] = h.get(line, "суммаввалютесчёта")
		row[stCurrency] = h.get(line, "валютасчёта")
		row[stCategory] = strings.TrimSpace(h.get(line, "вашакатегория"))
		if row[stCategory] == "" {
			row[stCategory] = h.get(line, "категорияпо-умолчанию")
		}
		row[stCounterparty] = h.get(line, "описание")
		row[stNote] = strings.TrimSpace(h.get(line, "сообщение"))
		if paid := strings.TrimSpace(h.get(line, "валютаоперации")); paid != "" && !strings.EqualFold(paid, strings.TrimSpace(row[stCurrency])) {
			row[stNote] = joinNote(row[stNote], strings.TrimLeft(strings.TrimSpace(h.get(line, "суммаоперации")), "+-−")+" "+paid)
		}
		row[stMCC] = h.get(line, "mcc")
		row[stAccount] = h.get(line, "имясчёта")
		next := Line{Number: line.Number, Cells: row}
		if status := strings.TrimSpace(h.get(line, "статус")); status != "" && !isOK(status) {
			next.Declined = status
		}
		out.Rows = append(out.Rows, next)
	}
	return out
}

// isOK is a status saying the operation went through, in Cyrillic or Latin.
func isOK(status string) bool {
	s := strings.ToLower(status)
	return s == "ок" || s == "ok"
}

func joinNote(note, more string) string {
	if note == "" {
		return more
	}
	return note + " · " + more
}
