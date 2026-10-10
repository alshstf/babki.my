package table

import (
	"bytes"
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/xuri/excelize/v2"

	"babki.my/babki/internal/account"
	"babki.my/babki/internal/instrument"
)

// papers is a catalog that knows the papers by ticker and ISIN.
type papers map[string]instrument.Instrument

func (p papers) ByISIN(_ context.Context, isin string) (instrument.Instrument, error) {
	return p.find(func(in instrument.Instrument) bool { return in.ISIN == isin })
}

func (p papers) ByTickerTradable(_ context.Context, ticker string) (instrument.Instrument, error) {
	return p.find(func(in instrument.Instrument) bool { return in.Ticker == ticker })
}

func (p papers) find(match func(instrument.Instrument) bool) (instrument.Instrument, error) {
	for _, in := range p {
		if match(in) {
			return in, nil
		}
	}
	return instrument.Instrument{}, pgx.ErrNoRows
}

func (papers) Create(context.Context, instrument.Instrument) (instrument.Instrument, error) {
	panic("a preview files nothing")
}

func paper(ticker, isin string) instrument.Instrument {
	return instrument.Instrument{ID: uuid.New(), Ticker: ticker, ISIN: isin, Currency: "RUB"}
}

var catalogOfTests = papers{
	"SBER": paper("SBER", "RU0009029540"),
	"GAZP": paper("GAZP", "RU0007661625"),
	"OFZ":  paper("SU26238RMFS4", "RU000A1038V6"),
}

// preview reads a file against a ruble account whose catalog knows SBER, GAZP
// and an OFZ, the mapping guessed.
func preview(t *testing.T, content string, format Format) Preview {
	t.Helper()
	acc := account.WithBalance{Account: account.Account{ID: uuid.New(), Currency: "RUB", Type: account.TypeBrokerage}}
	svc := NewService(oneAccount{acc}, catalogOfTests, storedJournal(nil), &takesAll{}, nil, nil, noCategories{})
	p, err := svc.Preview(t.Context(), uuid.New(), acc.ID, content, format, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// row is a preview row as the test compares it: the line, the verdict, and the
// operation's type and amount or the reason.
func row(r Row) string {
	var b strings.Builder
	b.WriteString(strings.Join([]string{strconv.Itoa(r.Line.Number), string(r.Verdict)}, " "))
	if r.Operation != nil {
		b.WriteString(" " + string(r.Operation.Type) + " " + strconv.Itoa(int(r.Operation.AmountMinor)))
		if r.Operation.FeeMinor != 0 {
			b.WriteString(" fee " + strconv.Itoa(int(r.Operation.FeeMinor)))
		}
	}
	if r.Reason != nil {
		b.WriteString(" " + string(r.Reason.Code) + " " + r.Reason.Value)
	}
	return b.String()
}

func rows(p Preview) []string {
	out := make([]string, len(p.Rows))
	for i, r := range p.Rows {
		out[i] = row(r)
	}
	return out
}

func sameRows(t *testing.T, got Preview, want []string) {
	t.Helper()
	if g := rows(got); !slices.Equal(g, want) {
		t.Errorf("rows =\n  %s\nwant\n  %s", strings.Join(g, "\n  "), strings.Join(want, "\n  "))
	}
}

// An Excel workbook is read as its cells hold their values, not as the sheet
// shows them: a date as a date whatever its format, a number in full and
// ungrouped, a text of digits as it is; rows are numbered as Excel numbers
// them, a hidden sheet passed over.
func TestAWorkbookIsReadAsItsCellsHold(t *testing.T) {
	f := excelize.NewFile()
	_ = f.SetSheetName("Sheet1", "Старое")
	_ = f.SetCellValue("Старое", "A1", "не читать")
	_ = f.SetCellValue("Старое", "A2", "совсем")
	const s = "Операции"
	sheet, _ := f.NewSheet(s)
	f.SetActiveSheet(sheet)
	if err := f.SetSheetVisible("Старое", false); err != nil {
		t.Fatal(err)
	}
	for i, v := range []string{"Дата", "Операция", "Сумма", "Счёт", "Время"} {
		ref, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = f.SetCellValue(s, ref, v)
	}
	ru, _ := f.NewStyle(&excelize.Style{CustomNumFmt: ptr("[$-419]dd.mm.yyyy")})
	builtIn, _ := f.NewStyle(&excelize.Style{NumFmt: 14})
	grouped, _ := f.NewStyle(&excelize.Style{CustomNumFmt: ptr(`#,##0 "₽"`)})
	twoPlaces, _ := f.NewStyle(&excelize.Style{NumFmt: 2})
	_ = f.SetCellValue(s, "A2", 46035.0) // 2026-01-13
	_ = f.SetCellStyle(s, "A2", "A2", ru)
	_ = f.SetCellValue(s, "B2", "Пополнение")
	_ = f.SetCellValue(s, "C2", 1234.0)
	_ = f.SetCellStyle(s, "C2", "C2", grouped)
	_ = f.SetCellValue(s, "D2", "0012345")
	// Row 3 is blank; row 4 a sum Excel kept with a float's noise, shown
	// rounded, and a date with a time.
	_ = f.SetCellValue(s, "A4", 46036.75)
	_ = f.SetCellStyle(s, "A4", "A4", builtIn)
	_ = f.SetCellValue(s, "B4", "Пополнение")
	_ = f.SetCellValue(s, "C4", 0.1+0.2)
	_ = f.SetCellStyle(s, "C4", "C4", twoPlaces)
	_ = f.SetCellValue(s, "E4", 0.5)
	var b bytes.Buffer
	if err := f.Write(&b); err != nil {
		t.Fatal(err)
	}

	got, err := ReadWorkbook(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{
		{Number: 1, Cells: []string{"Дата", "Операция", "Сумма", "Счёт", "Время"}},
		{Number: 2, Cells: []string{"2026-01-13", "Пополнение", "1234", "0012345"}},
		{Number: 4, Cells: []string{"2026-01-14 18:00:00", "Пополнение", "0.3", "", "0.5"}},
	}
	if len(got.Rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", got.Rows, want)
	}
	for i := range want {
		if got.Rows[i].Number != want[i].Number || !slices.Equal(got.Rows[i].Cells, want[i].Cells) {
			t.Errorf("row %d = %+v, want %+v", i, got.Rows[i], want[i])
		}
	}

	if _, err := ReadWorkbook([]byte("Дата;Сумма\n")); err == nil {
		t.Error("a CSV was read as a workbook")
	}
	if _, err := Read("not base64!", FormatXLSX); err == nil {
		t.Error("a workbook that is not base64 was read")
	}
}

// The template reads back, as a CSV and as a workbook, into the same
// operations — each of its rows new, none left for the person to fix.
func TestTheTemplateReadsBack(t *testing.T) {
	asCSV := preview(t, string(TemplateCSV()), FormatText)
	workbook, err := TemplateXLSX()
	if err != nil {
		t.Fatal(err)
	}
	asXLSX := preview(t, base64.StdEncoding.EncodeToString(workbook), FormatXLSX)
	want := []string{
		"2 new deposit 10000000",
		"3 new buy -3055000 fee 1528",
		"4 new sell 1248000 fee 624",
		"5 new dividend 204900",
		"6 new tax -26637",
		"7 new interest 41260",
		"8 new fee -19900",
		"9 new withdrawal -2000000",
	}
	sameRows(t, asCSV, want)
	sameRows(t, asXLSX, want)
	if len(asCSV.Mapping.Columns) != len(templateHeader) {
		t.Errorf("the template's columns guessed = %v", asCSV.Mapping.Columns)
	}
	for i := range asCSV.Rows {
		a, x := asCSV.Rows[i].Operation, asXLSX.Rows[i].Operation
		if a == nil || x == nil || !a.OccurredOn.Equal(x.OccurredOn) || a.Note != x.Note {
			t.Errorf("row %d: the CSV and the workbook differ: %+v / %+v", i, a, x)
		}
	}
}

// Snowball Income's table: the money of a deposit or a dividend is in
// Quantity, the tax withheld from a dividend becomes a tax of its own on the
// same line, a fee is in FeeTax; what a table cannot hold is shown unread.
func TestASnowballExportIsRead(t *testing.T) {
	p := preview(t, "Event,Date,Symbol,Price,Quantity,Currency,FeeTax,Exchange,FeeCurrency,DoNotAdjustCash,Note\n"+
		"Cash_In,2026-01-10,RUB,1,50000,RUB,,,,,\n"+
		"Buy,2026-01-12,SBER,300.5,100,RUB,15.03,MCX,RUB,,\n"+
		"Dividend,2026-02-16,SBER,34.15,3415,RUB,444,,,,\n"+
		"Fee,2026-03-01,,,0,RUB,199,,,,обслуживание\n"+
		"Cash_Gain,2026-03-02,RUB,1,12.5,RUB,,,,,\n"+
		"Split,2026-03-03,SBER,,10,RUB,,,,,\n", FormatText)
	if p.Tracker != TrackerSnowball {
		t.Errorf("tracker = %q", p.Tracker)
	}
	sameRows(t, p, []string{
		"2 new deposit 5000000",
		"3 new buy -3005000 fee 1503",
		"4 new dividend 341500",
		"4 new tax -44400",
		"5 new fee -19900",
		"6 new interest 1250",
		"7 unparsed type_not_mapped Split",
	})
}

// Ghostfolio's JSON export: a dividend and interest are quantity × unit
// price, a fee is its fee, the account named; a paper of Ghostfolio's own
// (MANUAL) is not looked for.
func TestAGhostfolioExportIsRead(t *testing.T) {
	p := preview(t, `{"meta":{"version":"2.150.0"},
	 "accounts":[{"id":"a1","name":"Брокер","currency":"RUB"}],
	 "activities":[
	  {"accountId":"a1","comment":null,"fee":12.5,"quantity":10,"type":"BUY","unitPrice":301.25,"currency":"RUB","dataSource":"MOEX","date":"2026-01-12T00:00:00.000Z","symbol":"SBER"},
	  {"accountId":"a1","comment":"див","fee":0,"quantity":10,"type":"DIVIDEND","unitPrice":34.15,"currency":"RUB","dataSource":"MOEX","date":"2026-02-16T00:00:00.000Z","symbol":"SBER"},
	  {"accountId":"a1","fee":99,"quantity":0,"type":"FEE","unitPrice":0,"currency":"RUB","dataSource":"MANUAL","date":"2026-03-01T00:00:00.000Z","symbol":"5f1c-uuid"},
	  {"accountId":"a1","fee":0,"quantity":1,"type":"INTEREST","unitPrice":41.26,"currency":"RUB","dataSource":"MANUAL","date":"2026-03-02T00:00:00.000Z","symbol":"7a2d-uuid"},
	  {"accountId":"a1","fee":0,"quantity":1,"type":"LIABILITY","unitPrice":1000,"currency":"RUB","dataSource":"MANUAL","date":"2026-03-03T00:00:00.000Z","symbol":"loan"}
	 ]}`, FormatText)
	if p.Tracker != TrackerGhostfolio {
		t.Errorf("tracker = %q", p.Tracker)
	}
	sameRows(t, p, []string{
		"1 new buy -301250 fee 1250",
		"2 new dividend 34150",
		"3 new fee -9900",
		"4 new interest 4126",
		"5 unparsed type_not_mapped LIABILITY",
	})
	if p.Header[len(p.Header)-1] != "Счёт" || p.Rows[0].Line.Cells[len(p.Header)-1] != "Брокер" {
		t.Errorf("the account is not named: %v / %v", p.Header, p.Rows[0].Line.Cells)
	}
	if _, err := Read(`{"rows":[]}`, FormatText); err == nil {
		t.Error("a JSON file that is not Ghostfolio's was read")
	}
}

// Intelinvest's CSV: no header, «#» lines of its own, the money a trade or a
// dividend moved written as a row of its own with the same link — shown, not
// imported, so it does not count twice; a bond priced in percent of its
// nominal, its accrued coupon paid with it; a dividend as the money that
// reached the account.
func TestAnIntelinvestExportIsRead(t *testing.T) {
	p := preview(t, "#CsvFormatVersion:v1\n"+
		"#AssetsDefinitionsStart.v1\n"+
		"STOCK;MYCO;Моя компания;100;RUB;;;;\n"+
		"#AssetsDefinitionsEnd\n"+
		"\n"+
		"MONEYDEPOSIT;10.01.2026 10:00:00;;;100000;;;;RUB;;Ввод;;\n"+
		"STOCKBUY;12.01.2026 11:00:00;GAZP;100;150.5;7.5;;;RUB;RUB;;L1;\n"+
		"MONEYWITHDRAW;12.01.2026 11:00:00;;;15057.5;;;;RUB;;Оплата;L1;\n"+
		"BONDBUY;13.01.2026 12:00:00;SU26238RMFS4:RU000A1038V6;2;98.5;1;12.3;1000;RUB;RUB;;L2;\n"+
		"MONEYWITHDRAW;13.01.2026 12:00:00;;;1983.3;;;;RUB;;;L2;\n"+
		"DIVIDEND;20.07.2026 00:00:00;GAZP;100;10;;;;RUB;;;L3;\n"+
		"MONEYDEPOSIT;20.07.2026 00:00:00;;;870;;;;RUB;;;L3;\n"+
		"BONDBUY;21.07.2026 00:00:00;SU26238RMFS4;1;99;;;;RUB;;;;\n"+
		"CURRENCY_BUY;22.07.2026 00:00:00;USD;10;90;;;;RUB;;;L4;\n", FormatText)
	if p.Tracker != TrackerIntelinvest {
		t.Fatalf("tracker = %q", p.Tracker)
	}
	sameRows(t, p, []string{
		"6 new deposit 10000000",
		"7 new buy -1505000 fee 750",
		"8 unparsed paired 7",
		"9 new buy -198230 fee 100",
		"10 unparsed paired 9",
		"11 new dividend 87000",
		"12 unparsed paired 11",
		"13 unparsed bad_number 99%",
		"14 unparsed type_not_mapped CURRENCY_BUY",
	})
}
