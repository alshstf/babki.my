// Package tradingmode says what this program is willing to call a trading
// mode — the broker's `classCode`, the board an order was matched on or the
// venue a deal was struck away from an order book.
//
// IT IS A LEAF PACKAGE ON PURPOSE. Both the journal (which stores the code on
// every imported row) and the importer (which shows the broker's own rows)
// have to name a mode, and the importer already depends on the journal — so a
// table living in either of them would be a table the other could not read.
package tradingmode

// Kind is what this program is willing to SAY about a broker's
// trading mode, and the set is deliberately small.
//
// The broker sends a bare code — "TQBR", "CNGD", "FINEX_OTC" — which means
// nothing to a reader, so something has to be said about it. What must NOT
// happen is that something be invented: a code this program cannot source is
// published as the code itself under Unknown, and the screen says
// in as many words that it does not know what it is. Four times in this
// project's history a true number has been published under a false caption;
// a made-up venue would be the fifth.
type Kind string

const (
	// OrderBook: an exchange's order book — an anonymous
	// ("безадресная") mode where the exchange matched the order against
	// whoever was on the other side.
	OrderBook Kind = "order_book"

	// Negotiated: an addressed ("адресная") mode of an exchange —
	// a deal struck with a named counterparty and registered by the exchange.
	// STILL ON THE EXCHANGE, which is why it is not OffExchange:
	// the owner's 52 currency deals in this mode were organised by the Moscow
	// Exchange, and calling them off-exchange would be false.
	Negotiated Kind = "negotiated"

	// OffExchange: dealing away from an exchange altogether.
	OffExchange Kind = "off_exchange"

	// Unknown: the broker named a mode and this program has no
	// source for what it is. The code travels to the screen unchanged and the
	// reader is told that much — see the package's table below for what
	// counts as a source.
	Unknown Kind = "unknown"
)

// evidence records WHY a row of the table below says what it says.
// It is not published; it exists so that a person changing the table has to
// answer the question the table exists to answer.
type Evidence string

const (
	// MoexISS: the code is a board in the Moscow Exchange's own
	// published board index (iss.moex.com/iss/index.json?iss.only=boards),
	// whose `board_title` names it — and the title's own last word says which
	// kind it is: "безадрес." for an order book, "адрес." for a negotiated
	// deal. The kind is therefore READ from the exchange's words rather than
	// judged: the titles, verbatim as ISS returned them on 2026-08-24, are
	// quoted against each row.
	MoexISS Evidence = "moex_iss"

	// CodeNamesItself: the code states its own nature and nothing has
	// to be inferred. Exactly one code qualifies — FINEX_OTC — and "OTC" is
	// the broker's word, not this program's reading of where those deals felt
	// like they happened. The broker's own documentation confirms it runs
	// over-the-counter dealing as a thing distinct from exchange trading
	// (developer.tbank.ru/invest/intro/useful-info/markets lists
	// «Внебиржевая торговля» among its markets), which is what makes the
	// suffix a statement rather than a coincidence.
	CodeNamesItself Evidence = "code_names_itself"
)

// Fact is one row of the table: what this program says about a code, and the
// citation for saying it. The citation is EXPORTED and read by a test, not
// left as a comment — a claim whose evidence nothing checks is a claim that
// drifts away from its evidence in silence.
type Fact struct {
	Kind Kind
	// Why is the authority behind Kind.
	Why Evidence
	// MoexTitle is the exchange's own title for its own board, verbatim, and
	// it is the evidence for Kind whenever Why is MoexISS: the title's last
	// word says «безадрес.» or «адрес.», which is what OrderBook and
	// Negotiated mean. It is NOT published to a client: every visible string
	// in this program is Russian through the frontend's t(), and a title
	// served from Go would be a second place where the interface speaks.
	MoexTitle string
}

// Modes is everything this program can say about a broker's trading
// mode. ABSENCE FROM THIS TABLE IS THE ANSWER "we do not know", not an
// oversight: Of returns Unknown for anything not here,
// the code itself still reaches the screen, and the reader is told plainly
// that it is unnamed. There is deliberately no second list of "codes we do not
// recognise" to keep in step with this one.
//
// WHAT IS NOT HERE, AND WHY, since the owner's own account carries all of it:
//
//   - SPBXM (11 trades in AMZN, MSFT, NVDA, PFE, SLB, STLA, plus 52
//     dividends) and SPBOPT (2 option operations). The prefix looks like the
//     Saint Petersburg Exchange and probably is, but neither code appears in
//     any board index this program can read, and the broker's own
//     documentation names its venues without ever mapping them to class
//     codes. "Probably" is not a source.
//   - BQUOTE_SHR (a purchase of AT&T) and A29 (a sale of Carnival), both on
//     2023-02-16, both in foreign shares after exchange trading in them had
//     stopped. The shape of the data suggests off-exchange dealing; the shape
//     of the data is not a citation.
//   - PSSU (one purchase). PSAU and PSBB, which look like its siblings, ARE
//     in the exchange's index; PSSU is not, and the resemblance of three
//     letters is not evidence about the fourth.
//   - FAKE_OLD_MEX (the three 2021 purchases of the TCS receipts, from before
//     the redomiciliation). The broker's own placeholder, whose name says it
//     stands in for something rather than saying what.
//
// Each of these would be easy to label and the label would be a guess. The
// route to naming them is the broker's instrument passport, which returns a
// classCode beside an instrument's exchange — evidence from the same source
// that produced the code. Until that is asked for, the honest answer is the
// code and a shrug.
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
	// The currency market's own two, and they are a matched pair: the same
	// exchange runs both, one through the order book and one not.
	"CETS": {OrderBook, MoexISS, "Системные сделки - безадрес."},
	"CNGD": {Negotiated, MoexISS, "Внесистемные сделки- адрес."},
	// Primary placement and buyback: addressed, and organised by the exchange.
	"PSAU": {Negotiated, MoexISS, "Размещение - адрес."},
	"PSBB": {Negotiated, MoexISS, "Выкуп - адрес."},

	// The broker's own over-the-counter dealing in the FinEx funds, opened
	// after exchange trading in them stopped. The one code in this table
	// whose evidence is its own name.
	"FINEX_OTC": {OffExchange, CodeNamesItself, ""},
}

// Of says what kind of mode a broker's code names, or
// Unknown when this program has no source for it — including for
// the empty code, which is what the broker sends for an operation that
// describes no instrument at all (money in and out of the account).
//
// THE CODE IS NEVER TRANSLATED AWAY. Callers publish it beside this answer, so
// a reader of an unknown mode sees exactly what the broker said and is told
// that this program cannot name it.
func Of(classCode string) Kind {
	if fact, ok := Modes[classCode]; ok {
		return fact.Kind
	}
	return Unknown
}
