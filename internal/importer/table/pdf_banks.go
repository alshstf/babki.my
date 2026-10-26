package table

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"babki.my/babki/internal/operation"
)

// The banks whose statements in PDF this package reads. Сбер, Газпромбанк and
// Альфа-Банк give a person no table of their operations, only a PDF.
const (
	TrackerSber        Tracker = "sber"
	TrackerGazprombank Tracker = "gazprombank"
	TrackerAlfa        Tracker = "alfa"
)

var (
	pdfDay      = regexp.MustCompile(`^\d{2}\.\d{2}\.\d{4}$`)
	pdfDayTime  = regexp.MustCompile(`^\d{2}\.\d{2}\.\d{4}(?: \d{2}:\d{2})?$`)
	pdfTime     = regexp.MustCompile(`^\d{2}:\d{2}$`)
	pdfMoney    = regexp.MustCompile(`^[+\-−]?\d{1,3}(?:[ \x{00A0}\x{202F}]\d{3})*,\d{2}$`)
	pdfAccount  = regexp.MustCompile(`\d{5}[ \x{00A0}]?\d{3}[ \x{00A0}]?\d[ \x{00A0}]?\d{4}[ \x{00A0}]?\d{7}`)
	pdfCardTail = regexp.MustCompile(`\.?\s*Операция по (?:счету|карте)\s*\*{4}\d{4}\.?\s*$`)
	// pdfPaid is an amount in the operation's own currency, with its sign.
	pdfPaid = regexp.MustCompile(`^\d[\d  ]*,\d{2}\b`)
)

// readPDF reads a bank's statement in PDF: which bank's it is by its own
// words, then that bank's layout. A PDF of no bank known here — a
// certificate, a receipt, another bank's statement — is ErrBadTable.
func readPDF(data []byte) (Table, error) {
	pages, err := pdfPages(data)
	if err != nil {
		return Table{}, err
	}
	return fromPDFPages(pages)
}

func fromPDFPages(pages [][]pdfRow) (Table, error) {
	var b strings.Builder
	for _, page := range pages {
		for _, row := range page {
			b.WriteString(row.text())
			b.WriteByte('\n')
		}
	}
	all := b.String()
	switch {
	case strings.Contains(all, "sberbank.ru") && strings.Contains(all, "Расшифровка операций"):
		return fromSberPDF(pages)
	case strings.Contains(all, "GAZPRUMM") && strings.Contains(all, "Отчет по операциям"):
		return fromGazprombankPDF(pages)
	case strings.Contains(all, "АЛЬФА-БАНК") && strings.Contains(all, "Операции по счету"):
		return fromAlfaPDF(pages)
	}
	return Table{}, fmt.Errorf("%w: the PDF is not a bank's statement this program reads", ErrBadTable)
}

// statement collects a PDF's operations into a bank statement's columns.
type statement struct{ t Table }

func newStatement(tracker Tracker) *statement {
	return &statement{t: Table{Tracker: tracker, Rows: []Line{{Number: 1, Cells: statementHeader}}}}
}

func (s *statement) add(row []string, line Line) {
	line.Number = len(s.t.Rows) + 1
	line.Cells = row
	s.t.Rows = append(s.t.Rows, line)
}

func (s *statement) table() (Table, error) {
	if len(s.t.Rows) < 2 {
		return Table{}, fmt.Errorf("%w: no operations found in the statement", ErrBadTable)
	}
	if len(s.t.Rows) > MaxRows+1 {
		return Table{}, fmt.Errorf("%w: more than %d rows", ErrBadTable, MaxRows)
	}
	return s.t, nil
}

// continues says whether a row goes on the operation whose last row is at
// prev: the next line of its text, a line's height below. Operations do not
// run over a page.
func continues(prev float64, row pdfRow) bool {
	return prev-row.Y <= 16
}

// signed is a money cell as a statement means it: «+» or nothing before an
// amount coming in, as the bank writes it; out turns an unsigned amount into
// money out.
func signed(cell string, out bool) string {
	cell = strings.TrimSpace(cell)
	switch {
	case strings.HasPrefix(cell, "+"):
		return strings.TrimPrefix(cell, "+")
	case strings.HasPrefix(cell, "-"), strings.HasPrefix(cell, "−"):
		return cell
	case out:
		return "-" + cell
	}
	return cell
}

// fromSberPDF reads Сбер's statements — of a payment account, of a credit
// card's account, and the credit card's monthly report. An operation is two
// lines and its description may run on: the day, the time, Сбер's category,
// the amount (money in with «+») and the balance after; then the day it was
// processed, the authorization code and the description; an amount in another
// currency stands under the account's on the second line.
func fromSberPDF(pages [][]pdfRow) (Table, error) {
	s := newStatement(TrackerSber)
	account := sberAccount(pages)
	for _, page := range pages {
		var (
			row  []string
			last float64
			step int // 0 — no operation open, 1 — first line read, 2 — second
		)
		flush := func() {
			if row != nil {
				row[stCounterparty] = pdfCardTail.ReplaceAllString(strings.Join(strings.Fields(row[stCounterparty]), " "), "")
				s.add(row, Line{})
			}
			row, step = nil, 0
		}
		for _, r := range page {
			c := r.Cells
			switch {
			case len(c) >= 3 && pdfDay.MatchString(c[0].Text) && pdfTime.MatchString(c[1].Text):
				flush()
				var money []string
				var category []string
				for _, cell := range c[2:] {
					if pdfMoney.MatchString(cell.Text) {
						money = append(money, cell.Text)
					} else {
						category = append(category, cell.Text)
					}
				}
				if len(money) == 0 {
					continue
				}
				amount := money[0]
				if len(money) >= 2 {
					amount = money[len(money)-2]
				}
				row = make([]string, len(statementHeader))
				row[stDate] = c[0].Text
				row[stAmount] = signed(amount, true)
				row[stCurrency] = "RUB"
				row[stCategory] = strings.Join(category, " ")
				row[stAccount] = account
				step, last = 1, r.Y
			case step == 1 && len(c) >= 2 && pdfDay.MatchString(c[0].Text) && continues(last, r):
				var text, paid []string
				for _, cell := range c[2:] {
					if cell.X > 300 && pdfPaid.MatchString(cell.Text) {
						paid = append(paid, cell.Text)
					} else {
						text = append(text, cell.Text)
					}
				}
				row[stCounterparty] = strings.Join(text, " ")
				row[stNote] = strings.Join(paid, " ")
				step, last = 2, r.Y
			case step == 2 && len(c) > 0 && c[0].X > 100 && continues(last, r) && !sberFooter(r):
				row[stCounterparty] += " " + r.text()
				last = r.Y
			default:
				flush()
			}
		}
		flush()
	}
	return s.table()
}

// sberFooter is a line of Сбер's page footer.
func sberFooter(r pdfRow) bool {
	t := r.text()
	return strings.HasPrefix(t, "Продолжение на следующей") || strings.HasPrefix(t, "Для проверки подлинности")
}

// sberAccount is the statement's account number, or — in the credit card's
// report, which gives none — its card.
func sberAccount(pages [][]pdfRow) string {
	card := ""
	for _, page := range pages {
		for _, r := range page {
			if len(r.Cells) < 2 {
				continue
			}
			switch r.Cells[0].Text {
			case "Номер счёта", "Номер счета":
				return strings.Join(strings.Fields(r.Cells[1].Text), "")
			case "Карта":
				if card == "" {
					card = r.Cells[1].Text
				}
			}
		}
	}
	return card
}

// pdfColumn is a column of a statement's table: what it holds and where it
// begins and ends on the page.
type pdfColumn struct {
	kind     string
	from, to float64
}

// tableColumns reads a statement table's columns off its header — the row
// naming the operation's content and the few under it, which may name
// money in and out — each column running to the middle between its heading
// and the next, as text in narrow columns stands centred.
func tableColumns(header []pdfRow, kinds func(string) string) []pdfColumn {
	type head struct {
		kind string
		x    float64
	}
	var heads []head
	seen := map[string]bool{}
	for _, r := range header {
		for _, c := range r.Cells {
			if k := kinds(c.Text); k != "" && !seen[k] {
				seen[k] = true
				heads = append(heads, head{k, c.X})
			}
		}
	}
	for i := 1; i < len(heads); i++ {
		for j := i; j > 0 && heads[j].x < heads[j-1].x; j-- {
			heads[j], heads[j-1] = heads[j-1], heads[j]
		}
	}
	out := make([]pdfColumn, len(heads))
	for i, h := range heads {
		out[i] = pdfColumn{kind: h.kind, from: math.Inf(-1), to: math.Inf(1)}
		if i > 0 {
			out[i].from = (heads[i-1].x + h.x) / 2
		}
		if i+1 < len(heads) {
			out[i].to = (h.x + heads[i+1].x) / 2
		}
	}
	return out
}

func columnAt(cols []pdfColumn, x float64) string {
	for _, c := range cols {
		if x >= c.from && x < c.to {
			return c.kind
		}
	}
	return ""
}

// byColumn is a row's text gathered by column.
func byColumn(cols []pdfColumn, rows ...pdfRow) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		for _, c := range r.Cells {
			k := columnAt(cols, c.X)
			if out[k] == "" {
				out[k] = c.Text
			} else {
				out[k] += " " + c.Text
			}
		}
	}
	return out
}

func gazprombankColumn(text string) string {
	switch {
	case text == "Дата" || strings.HasPrefix(text, "Дата совершения"):
		return "day"
	case strings.HasPrefix(text, "Дата отражения"):
		return "posted"
	case strings.HasPrefix(text, "Номер карты"), strings.HasPrefix(text, "№ карты"), text == "№":
		return "card"
	case strings.HasPrefix(text, "Содержание операции"):
		return "content"
	case text == "Приход":
		return "in"
	case text == "Расход":
		return "out"
	case strings.HasPrefix(text, "Остаток"):
		return "balance"
	}
	return ""
}

// fromGazprombankPDF reads Газпромбанк's statements of a card and of a card's
// account, debit or credit alike. The table follows the account's details
// (and, for a credit card, its table of grace periods, whose rows start with
// days too); with no header read yet no column is known and no row is an
// operation. An
// operation starts with the day it was made and the day it reached the
// account; its content, and in narrow columns its amounts, run over several
// lines. The money comes in «Приход» and goes in «Расход» — signed in a debit
// card's statement, unsigned in a credit card's.
//
// A credit card's statement shows how the bank applied money that came in:
// «Погашение …» of the debt, the minimum payment, the interest. The debt's
// part moves money within the account — the money is counted where it came
// in — and is not loaded; the interest's is the bank's fee, the interest
// charged that the statement shows no other way.
func fromGazprombankPDF(pages [][]pdfRow) (Table, error) {
	s := newStatement(TrackerGazprombank)
	account := gazprombankAccount(pages)
	var cols []pdfColumn
	for _, page := range pages {
		var op []pdfRow
		flush := func() {
			if len(op) > 0 {
				gazprombankOperation(s, cols, account, op)
			}
			op = nil
		}
		for i, r := range page {
			if strings.Contains(r.text(), "Содержание операции") {
				flush()
				end := min(i+5, len(page))
				cols = tableColumns(page[i:end], gazprombankColumn)
				continue
			}
			v := byColumn(cols, r)
			switch {
			case pdfDayTime.MatchString(v["day"]) && pdfDayTime.MatchString(v["posted"]):
				flush()
				op = []pdfRow{r}
			case len(op) > 0 && continues(op[len(op)-1].Y, r) && v["day"] == "" && v["posted"] == "":
				op = append(op, r)
			default:
				flush()
			}
		}
		flush()
	}
	return s.table()
}

// gazprombankAccount is the statement's account: its number on the line
// naming it.
func gazprombankAccount(pages [][]pdfRow) string {
	for _, page := range pages {
		for _, r := range page {
			if t := r.text(); strings.HasPrefix(t, "Номер счета") {
				if n := pdfAccount.FindString(t); n != "" {
					return strings.Join(strings.Fields(n), "")
				}
			}
		}
	}
	return ""
}

var (
	gpbDevice = regexp.MustCompile(`Устройство:\s*(.+?)\.\s*Город:`)
	gpbPaid   = regexp.MustCompile(`Сумма операции:\s*([\d.]+)\.\s*Валюта операции:\s*(.+?)\.?\s*$`)
	gpbMCC    = regexp.MustCompile(`^[A-Z]+\*(\d{4})\*`)
)

func gazprombankOperation(s *statement, cols []pdfColumn, account string, op []pdfRow) {
	v := byColumn(cols, op...)
	content := strings.Join(strings.Fields(v["content"]), " ")
	var money []string
	for _, k := range []string{"in", "out"} {
		cell := strings.Join(strings.Fields(v[k]), " ")
		if cell == "" {
			continue
		}
		if d, _, err := parseNumber(cell, FieldAmount); err == nil && d.IsZero() {
			continue
		}
		money = append(money, signed(cell, k == "out"))
	}
	row := make([]string, len(statementHeader))
	row[stDate] = strings.Fields(v["day"])[0]
	row[stCurrency] = "RUB"
	row[stAccount] = account
	row[stCounterparty] = content
	if len(money) == 1 {
		row[stAmount] = money[0]
	} else {
		// Both columns with money, or neither: the reader names the cell.
		row[stAmount] = strings.Join(money, " ")
	}
	line := Line{}
	switch {
	case strings.HasPrefix(content, "Погашение") && strings.Contains(content, "процент"):
		line.Type = operation.TypeFee
		row[stCategory] = "Проценты по кредиту"
	case strings.HasPrefix(content, "Погашение"):
		line.Internal = content
	case strings.Contains(content, "Операция:"):
		// «Возврат части стоимости покупки. Операция: …» names itself before
		// the card's details; a purchase names its shop as the device.
		row[stCounterparty] = strings.Trim(content[:strings.Index(content, "Операция:")], " .")
		if m := gpbDevice.FindStringSubmatch(content); m != nil {
			if device := strings.Trim(m[1], " ."); device != "" {
				row[stCounterparty] = device
			}
			if code := gpbMCC.FindStringSubmatch(m[1]); code != nil {
				row[stMCC] = code[1]
			}
		}
		if row[stCounterparty] == "" {
			row[stCounterparty] = content
		}
		if m := gpbPaid.FindStringSubmatch(content); m != nil && !strings.Contains(m[2], "Российские рубли") {
			row[stNote] = m[1] + " " + m[2]
		}
	}
	s.add(row, line)
}

func alfaColumn(text string) string {
	switch text {
	case "Дата проводки":
		return "day"
	case "Код операции":
		return "code"
	case "Описание":
		return "content"
	case "Сумма":
		return "amount"
	}
	return ""
}

var alfaAmount = regexp.MustCompile(`^([+\-−]?\d{1,3}(?:[ \x{00A0}\x{202F}]\d{3})*,\d{2})\s+([A-Z]{3})$`)

// fromAlfaPDF reads Альфа-Банк's statement of an account: the day it was
// posted, the operation's code, its description over one or two lines, and
// the amount with its sign and currency.
func fromAlfaPDF(pages [][]pdfRow) (Table, error) {
	s := newStatement(TrackerAlfa)
	account := ""
	var cols []pdfColumn
	for _, page := range pages {
		var op []pdfRow
		flush := func() {
			if len(op) > 0 {
				v := byColumn(cols, op...)
				m := alfaAmount.FindStringSubmatch(strings.TrimSpace(v["amount"]))
				row := make([]string, len(statementHeader))
				row[stDate] = v["day"]
				row[stAccount] = account
				row[stCounterparty] = strings.TrimSuffix(strings.TrimSpace(v["content"]), " Без НДС.")
				row[stAmount] = v["amount"]
				if m != nil {
					row[stAmount], row[stCurrency] = m[1], m[2]
				}
				s.add(row, Line{})
			}
			op = nil
		}
		for _, r := range page {
			if len(r.Cells) >= 2 && r.Cells[0].Text == "Номер счета" && account == "" {
				account = r.Cells[1].Text
			}
			if r.Cells[0].Text == "Дата проводки" {
				cols = tableColumns([]pdfRow{r}, alfaColumn)
				continue
			}
			v := byColumn(cols, r)
			switch {
			case pdfDay.MatchString(v["day"]) && v["code"] != "":
				flush()
				op = []pdfRow{r}
			case len(op) > 0 && continues(op[len(op)-1].Y, r) && v["day"] == "" && v["code"] == "":
				op = append(op, r)
			default:
				flush()
			}
		}
		flush()
	}
	return s.table()
}
