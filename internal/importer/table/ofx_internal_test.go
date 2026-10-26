package table

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/category"
	"babki.my/babki/internal/operation"
)

// oneCategory is a family with a single category of spending, «Сервис».
type oneCategory struct{ category.Category }

func (c oneCategory) List(context.Context, uuid.UUID) ([]category.Category, error) {
	return []category.Category{c.Category}, nil
}

func (oneCategory) Rules(context.Context, uuid.UUID) ([]category.Rule, error) { return nil, nil }

// tbankOFX is the made-up statement of tbankStatement as Т-Банк's OFX writes
// it: version 2, XML, a statement per account, the bank's category in MEMO,
// the operation that did not go through left out.
const tbankOFX = `<?xml version="1.0" encoding="utf-8" ?>
<?OFX OFXHEADER="200" VERSION="202" SECURITY="NONE" OLDFILEUID="NONE" NEWFILEUID="NONE"?>
<OFX>
  <BANKMSGSRSV1>
    <STMTTRNRS>
      <STMTRS>
        <CURDEF>RUB</CURDEF>
        <BANKACCTFROM>
          <BANKID>T-BANK</BANKID>
          <ACCTID>40817810000000000001</ACCTID>
          <ACCTTYPE>CHECKING</ACCTTYPE>
        </BANKACCTFROM>
        <BANKTRANLIST>
          <STMTTRN>
            <TRNTYPE>DEBIT</TRNTYPE>
            <DTPOSTED>20261003101500.000[+3:MSK]</DTPOSTED>
            <TRNAMT>-1100.00</TRNAMT>
            <FITID>1001</FITID>
            <NAME>Облако</NAME>
            <MEMO>Сервис</MEMO>
            <CURRENCY><CURSYM>RUB</CURSYM><CURRATE>1.0</CURRATE></CURRENCY>
          </STMTTRN>
          <STMTTRN>
            <TRNTYPE>CREDIT</TRNTYPE>
            <DTPOSTED>20261004090000.000[+3:MSK]</DTPOSTED>
            <TRNAMT>5000.00</TRNAMT>
            <FITID>1002</FITID>
            <NAME>Иван И.</NAME>
            <MEMO>Переводы</MEMO>
          </STMTTRN>
        </BANKTRANLIST>
      </STMTRS>
    </STMTTRNRS>
    <STMTTRNRS>
      <STMTRS>
        <CURDEF>RUB</CURDEF>
        <BANKACCTFROM>
          <BANKID>T-BANK</BANKID>
          <ACCTID>42301810000000000002</ACCTID>
          <ACCTTYPE>SAVINGS</ACCTTYPE>
        </BANKACCTFROM>
        <BANKTRANLIST>
          <STMTTRN>
            <TRNTYPE>CREDIT</TRNTYPE>
            <DTPOSTED>20261006030000.000[+3:MSK]</DTPOSTED>
            <TRNAMT>4.47</TRNAMT>
            <FITID>1003</FITID>
            <NAME>Проценты на остаток</NAME>
            <MEMO>Проценты</MEMO>
          </STMTTRN>
        </BANKTRANLIST>
      </STMTRS>
    </STMTTRNRS>
  </BANKMSGSRSV1>
</OFX>
`

// A statement in OFX loads its chosen account the way Т-Банк's CSV does —
// the bank's category from MEMO, the shop as the counterparty — and the very
// same operations already loaded from the CSV are duplicates, not new: both
// files date them to the second alike, so the content says they are one.
func TestAnOFXStatementLoadsAsItsCSVAndKnowsItsRows(t *testing.T) {
	guessed := previewOn(t, tbankOFX, FormatText, nil)
	if guessed.Tracker != TrackerOFX {
		t.Fatalf("tracker = %q, want OFX", guessed.Tracker)
	}
	if want := []FileAccount{{"40817810000000000001", 2}, {"42301810000000000002", 1}}; !slices.Equal(guessed.FileAccounts, want) {
		t.Errorf("file accounts = %v, want %v", guessed.FileAccounts, want)
	}
	m := guessed.Mapping
	m.Account = "40817810000000000001"
	p := previewOn(t, tbankOFX, FormatText, &m)
	sameRows(t, p, []string{
		"2 new withdrawal -110000",
		"3 new deposit 500000",
		"4 unparsed other_account 42301810000000000002",
	})
	if op := p.Rows[0].Operation; op.Counterparty != "Облако" || op.Note != "Сервис" || op.OccurredOn.Format("2006-01-02") != "2026-10-03" {
		t.Errorf("purchase = %s, %q, note %q; want 2026-10-03, «Облако», the bank's category", op.OccurredOn.Format("2006-01-02"), op.Counterparty, op.Note)
	}

	// Т-Банк's MEMO is its category: a family that has one by that name
	// files the purchase under it.
	service := oneCategory{category.Category{ID: uuid.New(), Kind: category.KindExpense, Name: "Сервис"}}
	acc := account.WithBalance{Account: account.Account{ID: uuid.New(), Currency: "RUB", Type: account.TypeChecking}}
	filed, err := NewService(oneAccount{acc}, catalogOfTests, storedJournal(nil), &takesAll{}, nil, nil, service).
		Preview(t.Context(), uuid.New(), acc.ID, tbankOFX, FormatText, &m)
	if err != nil {
		t.Fatal(err)
	}
	if op := filed.Rows[0].Operation; op.CategoryID == nil || *op.CategoryID != service.ID || op.Note != "" {
		t.Errorf("purchase category = %v, note %q; want «Сервис» from MEMO and no note", op.CategoryID, op.Note)
	}

	csvMapping := previewOn(t, tbankStatement, FormatText, nil).Mapping
	csvMapping.Account = "Карта"
	var loaded []operation.Operation
	for _, r := range previewOn(t, tbankStatement, FormatText, &csvMapping).Rows {
		if r.Operation != nil {
			loaded = append(loaded, *r.Operation)
		}
	}
	svc := NewService(oneAccount{acc}, catalogOfTests, storedJournal(loaded), &takesAll{}, nil, nil, noCategories{})
	again, err := svc.Preview(t.Context(), uuid.New(), acc.ID, tbankOFX, FormatText, &m)
	if err != nil {
		t.Fatal(err)
	}
	sameRows(t, again, []string{
		"2 duplicate withdrawal -110000",
		"3 duplicate deposit 500000",
		"4 unparsed other_account 42301810000000000002",
	})
}

// OFX version 1 is SGML: values without closing tags, a transaction the next
// one closes. The day is the one the person paid (DTUSER) rather than the one
// the bank posted, a card purchase's code may come as SIC, and an amount in
// another currency than the statement's says so in CURRENCY — while
// ORIGCURRENCY only describes the original and leaves the amount in CURDEF.
func TestAnSGMLStatementIsReadAsTheBankMeantIt(t *testing.T) {
	const sgml = "OFXHEADER:100\r\nDATA:OFXSGML\r\nVERSION:102\r\nCHARSET:1252\r\n\r\n" +
		"<OFX><CREDITCARDMSGSRSV1><CCSTMTTRNRS><CCSTMTRS><CURDEF>EUR<CCACCTFROM><ACCTID>4111XXXXXXXX1111</CCACCTFROM>" +
		"<BANKTRANLIST>" +
		"<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20261005<DTUSER>20261003<TRNAMT>-42.10<FITID>A1<NAME>Cafe &amp; Bar<MEMO>Lunch<SIC>5812" +
		"<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20261006<TRNAMT>-15.00<FITID>A2<NAME>Shop<CURRENCY><CURSYM>USD<CURRATE>0.92</CURRENCY></STMTTRN>" +
		"<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20261007<TRNAMT>-9.20<FITID>A3<NAME>Store<ORIGCURRENCY><CURSYM>GBP<CURRATE>1.18</ORIGCURRENCY>" +
		"</BANKTRANLIST></CCSTMTRS></CCSTMTTRNRS></CREDITCARDMSGSRSV1></OFX>"
	p := previewOn(t, sgml, FormatText, nil)
	lunch := oneCategory{category.Category{ID: uuid.New(), Kind: category.KindExpense, Name: "Lunch"}}
	acc := account.WithBalance{Account: account.Account{ID: uuid.New(), Currency: "EUR", Type: account.TypeChecking}}
	other, err := NewService(oneAccount{acc}, catalogOfTests, storedJournal(nil), &takesAll{}, nil, nil, lunch).
		Preview(t.Context(), uuid.New(), acc.ID, sgml, FormatText, nil)
	if err != nil {
		t.Fatal(err)
	}
	if op := other.Rows[0].Operation; op.CategoryID != nil {
		t.Error("another bank's MEMO was taken for a category; it is the person's note")
	}
	sameRows(t, p, []string{
		"2 new withdrawal -4210",
		"3 new withdrawal -1500",
		"4 new withdrawal -920",
	})
	cafe, shop, store := p.Rows[0].Operation, p.Rows[1].Operation, p.Rows[2].Operation
	if cafe.OccurredOn.Format("2006-01-02") != "2026-10-03" || cafe.Counterparty != "Cafe & Bar" || cafe.Note != "Lunch" || cafe.MCC != "5812" || cafe.Currency != "EUR" {
		t.Errorf("cafe = %s %s %q %q MCC %q; want 2026-10-03 EUR «Cafe & Bar», «Lunch», 5812",
			cafe.OccurredOn.Format("2006-01-02"), cafe.Currency, cafe.Counterparty, cafe.Note, cafe.MCC)
	}
	if shop.Currency != "USD" || store.Currency != "EUR" {
		t.Errorf("currencies = %s and %s, want USD (CURRENCY) and EUR (ORIGCURRENCY keeps CURDEF)", shop.Currency, store.Currency)
	}
}

// A text that only looks like a table is not taken for OFX, and an OFX file
// with no transaction says so rather than load nothing quietly.
func TestOFXIsToldByItsHeader(t *testing.T) {
	if isOFX("Дата;Сумма\n01.10.2026;<OFX>\n") {
		t.Error("a CSV with «<OFX>» in a cell was taken for OFX")
	}
	if _, err := Read("<?xml version=\"1.0\"?><?OFX OFXHEADER=\"200\"?><OFX><BANKMSGSRSV1></BANKMSGSRSV1></OFX>", FormatText); err == nil {
		t.Error("an OFX file without transactions was read as an empty table")
	}
}
