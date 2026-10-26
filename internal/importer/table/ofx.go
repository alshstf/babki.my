package table

import (
	"fmt"
	"html"
	"strings"
)

// TrackerOFX is a statement in OFX, the open format banks export for
// accounting programs — Т-Банк among them, and many banks abroad: version 2 as
// XML, version 1 as SGML, whose values have no closing tags.
const TrackerOFX Tracker = "ofx"

// isOFX is an OFX statement by how it begins: version 1's header, version 2's
// XML declaration followed by OFX's, or the element itself — not a table that
// mentions OFX somewhere in a cell.
func isOFX(content string) bool {
	head := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(content, "\uFEFF")))
	if len(head) > 4096 {
		head = head[:4096]
	}
	switch {
	case strings.HasPrefix(head, "OFXHEADER:"), strings.HasPrefix(head, "<OFX>"):
		return true
	case strings.HasPrefix(head, "<?XML"):
		return strings.Contains(head, "<?OFX ") || strings.Contains(head, "<OFX>")
	}
	return false
}

// ofxTxn is one transaction of an OFX statement, with the account it is of.
type ofxTxn struct {
	account, currency, bankID        string
	posted, user, amount, name, memo string
	sic, cursym                      string
}

// readOFX rearranges an OFX statement into a bank statement's columns, a row
// for each transaction, in the file's order. The date is the day the person
// paid (DTUSER) when the bank gives it, else the day it posted; the account is
// the statement's own number, so a file of several accounts asks which to
// load. The money is in the statement's currency unless a transaction names
// its own. Т-Банк writes its category in MEMO — checked against its CSV of the
// same operations — so there MEMO is the category, elsewhere a note.
func readOFX(content string) (Table, error) {
	txns, err := ofxTransactions(content)
	if err != nil {
		return Table{}, err
	}
	out := Table{Tracker: TrackerOFX, Rows: []Line{{Number: 1, Cells: statementHeader}}}
	for i, x := range txns {
		row := make([]string, len(statementHeader))
		day := x.user
		if day == "" {
			day = x.posted
		}
		row[stDate] = ofxDay(day)
		row[stAmount] = x.amount
		row[stCurrency] = x.currency
		if x.cursym != "" {
			row[stCurrency] = x.cursym
		}
		row[stCounterparty] = x.name
		if strings.EqualFold(x.bankID, "T-BANK") {
			row[stCategory] = x.memo
		} else {
			row[stNote] = x.memo
		}
		row[stMCC] = x.sic
		row[stAccount] = x.account
		out.Rows = append(out.Rows, Line{Number: i + 2, Cells: row})
	}
	if len(out.Rows) > MaxRows+1 {
		return Table{}, fmt.Errorf("%w: more than %d rows", ErrBadTable, MaxRows)
	}
	return out, nil
}

// ofxDay is an OFX date-time, «20261013031454.000[+3:MSK]», as a day.
func ofxDay(v string) string {
	if len(v) < 8 {
		return v
	}
	return v[:4] + "-" + v[4:6] + "-" + v[6:8]
}

// ofxTransactions walks the statement's tags in order. A value is the text
// after its opening tag up to the next tag, so SGML's unclosed values and
// XML's closed ones read alike; aggregates are told by what they open and
// close. Unknown tags are passed over.
func ofxTransactions(content string) ([]ofxTxn, error) {
	start := strings.Index(strings.ToUpper(content), "<OFX>")
	if start < 0 {
		return nil, fmt.Errorf("%w: no <OFX> element", ErrBadTable)
	}
	var (
		out        []ofxTxn
		account    string
		currency   string
		bankID     string
		cur        *ofxTxn
		inCurrency bool
	)
	rest := content[start:]
	for {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			break
		}
		closeAt := strings.IndexByte(rest[open:], '>')
		if closeAt < 0 {
			break
		}
		tag := strings.ToUpper(strings.TrimSpace(rest[open+1 : open+closeAt]))
		rest = rest[open+closeAt+1:]
		value := rest
		if next := strings.IndexByte(rest, '<'); next >= 0 {
			value = rest[:next]
		}
		value = strings.TrimSpace(html.UnescapeString(value))
		switch tag {
		case "STMTRS", "CCSTMTRS":
			account, currency, bankID = "", "", ""
		case "CURDEF":
			currency = strings.ToUpper(value)
		case "BANKID":
			bankID = value
		case "ACCTID":
			account = value
		case "STMTTRN", "/STMTTRN", "/BANKTRANLIST":
			// SGML may leave a transaction unclosed; the next one or the
			// list's end closes it.
			if cur != nil {
				out = append(out, *cur)
			}
			cur = nil
			if tag == "STMTTRN" {
				cur = &ofxTxn{account: account, currency: currency, bankID: bankID}
			}
		case "CURRENCY", "ORIGCURRENCY":
			// Only CURRENCY moves the amount into another currency;
			// ORIGCURRENCY describes the original and leaves it in CURDEF.
			inCurrency = tag == "CURRENCY"
		case "/CURRENCY", "/ORIGCURRENCY":
			inCurrency = false
		}
		if cur == nil {
			continue
		}
		switch tag {
		case "DTPOSTED":
			cur.posted = value
		case "DTUSER":
			cur.user = value
		case "TRNAMT":
			cur.amount = value
		case "NAME":
			cur.name = value
		case "MEMO":
			cur.memo = value
		case "SIC":
			cur.sic = value
		case "CURSYM":
			if inCurrency {
				cur.cursym = strings.ToUpper(value)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no transactions in the statement", ErrBadTable)
	}
	return out, nil
}
