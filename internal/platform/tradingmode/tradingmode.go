// Package tradingmode names the broker's trading modes (classCode): the board
// an order was matched on, or the venue of a deal struck off the order book.
//
// It is a leaf package because both the journal and the importer name modes,
// and the importer depends on the journal.
package tradingmode

// Kind is what this program says about a trading mode. A code it has no
// source for is Unknown and is shown as the bare code, never given a guessed
// label.
type Kind string

const (
	// OrderBook is an exchange's anonymous order book (безадресный режим).
	OrderBook Kind = "order_book"
	// Negotiated is an exchange's addressed mode (адресный режим): a deal with a
	// named counterparty, registered by the exchange. It is still on the
	// exchange.
	Negotiated Kind = "negotiated"
	// OffExchange is dealing away from any exchange.
	OffExchange Kind = "off_exchange"
	// Unknown is a code this program has no source for.
	Unknown Kind = "unknown"
)

// Evidence is the authority behind a row of Modes. A test checks it.
type Evidence string

const (
	// MoexISS: the code is a board in the Moscow Exchange's board index
	// (iss.moex.com/iss/index.json?iss.only=boards), and the board title's last
	// word — «безадрес.» or «адрес.» — gives the kind.
	MoexISS Evidence = "moex_iss"
	// CodeNamesItself: the code states its nature. Only FINEX_OTC qualifies;
	// the broker documents over-the-counter dealing as a market of its own.
	CodeNamesItself Evidence = "code_names_itself"
)

// Fact is one row of Modes.
type Fact struct {
	Kind Kind
	Why  Evidence
	// MoexTitle is the exchange's title for the board, verbatim (ISS,
	// 2026-08-24), when Why is MoexISS. It is evidence, not display text.
	MoexTitle string
}

// Modes is every code this program can name. Codes seen on the owner's
// account but absent here (SPBXM, SPBOPT, BQUOTE_SHR, A29, PSSU, FAKE_OLD_MEX)
// have no source that says what they are; the broker's instrument passport
// would be one.
var Modes = map[string]Fact{
	// Moscow Exchange, anonymous boards. Titles from ISS, 2026-08-24.
	"TQBR": {OrderBook, MoexISS, "Т+: Акции и ДР - безадрес."},
	"TQCB": {OrderBook, MoexISS, "Т+: Облигации - безадрес."},
	"TQOB": {OrderBook, MoexISS, "Т+: Гособлигации - безадрес."},
	"TQOY": {OrderBook, MoexISS, "Т+: Облигации (CNY) - безадрес."},
	"TQRD": {OrderBook, MoexISS, "Т+: Облигации Д - безадрес."},
	"TQTD": {OrderBook, MoexISS, "Т+: ETF (USD) - безадрес."},
	"TQTF": {OrderBook, MoexISS, "Т+: ETF - безадрес."},
	"TQIF": {OrderBook, MoexISS, "Т+: Паи - безадрес."},
	// The currency market's pair: one through the order book, one not.
	"CETS": {OrderBook, MoexISS, "Системные сделки - безадрес."},
	"CNGD": {Negotiated, MoexISS, "Внесистемные сделки- адрес."},
	// Primary placement and buyback: addressed, and organised by the exchange.
	"PSAU": {Negotiated, MoexISS, "Размещение - адрес."},
	"PSBB": {Negotiated, MoexISS, "Выкуп - адрес."},

	// The broker's own over-the-counter dealing in the FinEx funds, opened
	// after exchange trading in them stopped.
	"FINEX_OTC": {OffExchange, CodeNamesItself, ""},
}

// Of returns the kind of a broker's mode code, or Unknown — including for the
// empty code the broker sends on money movements. Callers show the code
// beside it.
func Of(classCode string) Kind {
	if fact, ok := Modes[classCode]; ok {
		return fact.Kind
	}
	return Unknown
}
