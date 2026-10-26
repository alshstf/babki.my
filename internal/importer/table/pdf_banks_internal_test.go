package table

import (
	"slices"
	"strings"
	"testing"

	"babki.my/babki/internal/operation"
)

// at is a page row: its height and its runs as «x:text», where the text
// stands on the page as the bank's PDF draws it.
func at(y float64, runs ...string) pdfRow {
	r := pdfRow{Y: y}
	for _, run := range runs {
		x, text, _ := strings.Cut(run, ":")
		var v float64
		for _, d := range x {
			v = v*10 + float64(d-'0')
		}
		r.Cells = append(r.Cells, pdfCell{X: v, Text: text})
	}
	return r
}

// statementRows is a statement's rows as the tests compare them.
func statementRows(t *testing.T, pages [][]pdfRow) []string {
	t.Helper()
	tbl, err := fromPDFPages(pages)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range tbl.Rows[1:] {
		s := strings.Join(l.Cells, " | ")
		if l.Internal != "" {
			s += " | internal"
		}
		if l.Type != "" {
			s += " | " + string(l.Type)
		}
		out = append(out, s)
	}
	return out
}

func sameStatement(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("rows =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Сбер draws an operation in two lines: day, time, its category, the amount
// — money in with «+», out with none — and the balance after; then the day it
// was processed, the authorization code and the description, which may run
// on. Thousands are parted with a no-break space. The page's footer is not
// the description's continuation, and an operation does not run over a page.
func TestSbersStatementIsReadOperationByOperation(t *testing.T) {
	page1 := []pdfRow{
		at(813, "171:900", "191:www.sberbank.ru"),
		at(761, "45:Выписка по платёжному счёту"),
		at(668, "57:Номер счёта", "195:40817 810 0 0000 0000001"),
		at(571, "45:Расшифровка операций"),
		at(545, "45:ДАТА ОПЕРАЦИИ (МСК)", "152:КАТЕГОРИЯ", "370:СУММА В ВАЛЮТЕ СЧЁТА", "490:ОСТАТОК СРЕДСТВ"),
		at(495, "45:20.10.2026", "102:18:18", "152:Перевод СБП", "422:12\u00a0000,00", "544:0,00"),
		at(484, "45:20.10.2026", "102:496291", "152:Другой банк. Операция по счету ****0001"),
		at(467, "45:20.10.2026", "102:09:55", "152:Прочие операции", "418:+12\u00a0000,00", "520:12\u00a0000,00"),
		at(456, "45:20.10.2026", "102:663583", "152:Заработная плата. Операция по счету ****0001"),
		at(439, "45:18.10.2026", "102:13:44", "152:Рестораны и кафе", "431:860,00", "544:0,00"),
		at(428, "45:19.10.2026", "102:349330", "152:MOSCOW YM*KAFE. Операция по карте", "425:9,50 $"),
		at(417, "152:****0001"),
		at(406, "236:Продолжение на следующей странице"),
		at(71, "109:Для проверки подлинности документа"),
	}
	page2 := []pdfRow{
		at(819, "45:Выписка по платёжному счёту", "506:Страница 2 из 2"),
		at(795, "45:ДАТА ОПЕРАЦИИ (МСК)", "152:КАТЕГОРИЯ", "370:СУММА В ВАЛЮТЕ СЧЁТА", "490:ОСТАТОК СРЕДСТВ"),
		at(750, "45:01.10.2026", "102:10:00", "152:Перевод на карту", "418:+1\u00a0500,00", "520:1\u00a0500,00"),
		at(739, "45:01.10.2026", "102:000001", "152:Перевод от И. Иван Иванович. Операция по карте ****0001"),
	}
	sameStatement(t, statementRows(t, [][]pdfRow{page1, page2}), []string{
		"20.10.2026 | -12\u00a0000,00 | RUB | Перевод СБП | Другой банк |  |  | 40817810000000000001",
		"20.10.2026 | 12\u00a0000,00 | RUB | Прочие операции | Заработная плата |  |  | 40817810000000000001",
		"18.10.2026 | -860,00 | RUB | Рестораны и кафе | MOSCOW YM*KAFE | 9,50 $ |  | 40817810000000000001",
		"01.10.2026 | 1\u00a0500,00 | RUB | Перевод на карту | Перевод от И. Иван Иванович |  |  | 40817810000000000001",
	})
}

// Газпромбанк's credit card statement puts its table of grace periods, whose
// rows start with days too, above the operations: only the rows under the
// operations' header are operations. Money comes unsigned in «Приход» and
// «Расход». The bank's application of money that came in shows as
// «Погашение …»: of the debt, it moves money within the account and is not
// loaded; of the interest, it is the interest charged — the bank's fee. A
// purchase's shop is its device, and Яндекс's devices carry the MCC.
func TestGazprombanksCreditCardStatementTellsMoneyInFromItsApplication(t *testing.T) {
	page := []pdfRow{
		at(676, "43:S.W.I.F.T.:", "43:GAZPRUMM"),
		at(800, "33:Номер счета", "240:40817810000000000002"),
		at(720, "37:03.06.2026", "128:31.07.2026", "246:6", "343:30.11.2026", "475:1 000,00"),
		at(550, "27:Отчет по операциям"),
		at(525, "20:Дата совершения", "120:Дата отражения", "220:Номер карты", "320:Содержание операции", "520:Сумма операции", "660:Остаток по счету в валюте счета"),
		at(514, "20:операции", "120:операции", "520:(руб.)", "660:(собственные средства) /"),
		at(495, "520:Приход", "590:Расход"),
		at(455, "25:24.04.2026 19:16", "125:27.04.2026 00:00", "225:220001****0001", "325:Операция: ПОКУПКА (ВНЕШНЯЯ СЕТЬ РФ).", "595:1 890,00"),
		at(444, "325:Карта 2200********0001. Устройство:"),
		at(432, "325:YANDEX*5814*EDA. Город: MOSCOW. Сумма"),
		at(421, "325:операции: 1890.00. Валюта операции:"),
		at(410, "325:Российские рубли."),
		at(399, "25:27.04.2026 00:00", "125:27.04.2026 00:00", "325:Погашение льготной ссудной", "525:900,00"),
		at(388, "325:задолженности по договору"),
		at(377, "25:27.04.2026 00:00", "125:27.04.2026 00:00", "325:Погашение процентов на срочную", "525:100,00"),
		at(366, "325:ссудную задолженность (нал)"),
		at(355, "25:27.04.2026 00:00", "125:27.04.2026 00:00", "325:Перевод между своими счетами", "525:1 000,00"),
		at(344, "25:26.04.2026 17:27", "125:28.04.2026 00:00", "225:220001****0001", "325:Операция: ПOКУПКА (ВНЕШНЯЯ СЕТЬ РФ).", "595:12,00", "660:0,00 / -12,00"),
		at(333, "325:Карта 2200********0001. Устройство: . SHOP."),
		at(322, "325:Город: MOSCOW. Сумма операции: 0.15."),
		at(311, "325:Валюта операции: Доллар США."),
		at(45, "281:2"),
	}
	got := statementRows(t, [][]pdfRow{page})
	sameStatement(t, got, []string{
		"24.04.2026 | -1 890,00 | RUB |  | YANDEX*5814*EDA |  | 5814 | 40817810000000000002",
		"27.04.2026 | 900,00 | RUB |  | Погашение льготной ссудной задолженности по договору |  |  | 40817810000000000002 | internal",
		"27.04.2026 | 100,00 | RUB | Проценты по кредиту | Погашение процентов на срочную ссудную задолженность (нал) |  |  | 40817810000000000002 | " + string(operation.TypeFee),
		"27.04.2026 | 1 000,00 | RUB |  | Перевод между своими счетами |  |  | 40817810000000000002",
		"26.04.2026 | -12,00 | RUB |  | SHOP | 0.15 Доллар США |  | 40817810000000000002",
	})
}

// A debit card's statement is narrow: text stands centred in its column and
// an amount too long for it runs on to the next line («+150» over «000,00»),
// money comes signed in both columns and the card's own column closes the
// row. Each column is read whole, line by line.
func TestGazprombanksNarrowStatementReadsEachColumnWhole(t *testing.T) {
	page := []pdfRow{
		at(676, "43:S.W.I.F.T.:", "43:GAZPRUMM"),
		at(514, "70:Номер счета карты", "240:40817810000000000003"),
		at(402, "132:Отчет по операциям за период с 27.09.2026 по 26.10.2026"),
		at(680, "92:Дата", "161:Дата отражения", "279:Содержание операции", "415:Приход", "467:Расход", "514:№ карты"),
		at(667, "72:совершения", "153:операции по Счету"),
		at(611, "80:26.09.2026", "180:27.09.2026", "284:Операция: ПОКУПКА", "422:+0,00", "468:-143,97", "516:****0003"),
		at(599, "300:ЧЕРЕЗ C2B СБП"),
		at(586, "285:(ТЕРМИНАЛ БАНКА)."),
		at(573, "285:Устройство: SBP SHOP 1."),
		at(561, "296:Город: MOSKVA."),
		at(397, "80:27.09.2026", "180:27.09.2026", "285:Перевод между своими", "424:+150", "474:-0,00"),
		at(385, "321:счетами", "420:000,00"),
	}
	sameStatement(t, statementRows(t, [][]pdfRow{page}), []string{
		"26.09.2026 | -143,97 | RUB |  | SBP SHOP 1 |  |  | 40817810000000000003",
		"27.09.2026 | 150 000,00 | RUB |  | Перевод между своими счетами |  |  | 40817810000000000003",
	})
}

// Альфа-Банк's statement: the day posted, the operation's code, a
// description over one or two lines, the amount with its sign and currency.
func TestAlfasStatementTakesItsCurrencyFromTheAmount(t *testing.T) {
	page := []pdfRow{
		at(700, "28:Выписка по счету"),
		at(670, "37:Номер счета", "147:40817810000000000004"),
		at(487, "28:Операции по счету"),
		at(466, "27:Дата проводки", "96:Код операции", "188:Описание", "541:Сумма"),
		at(456, "511:в валюте счета"),
		at(444, "27:11.06.2026", "96:B011106260000001", "188:Внутрибанковский перевод между счетами, ИВАНОВ И. И.", "506:-350 000,00 RUR"),
		at(424, "27:11.06.2026", "96:C191106260000002", "188:Перевод C191106260000002 из другого банка по номеру телефона от", "508:350 000,00 RUR"),
		at(415, "188:отправителя Иван И. Без НДС."),
		at(100, "100:(подпись сотрудника АО «АЛЬФА-БАНК»)"),
	}
	sameStatement(t, statementRows(t, [][]pdfRow{page}), []string{
		"11.06.2026 | -350 000,00 | RUR |  | Внутрибанковский перевод между счетами, ИВАНОВ И. И. |  |  | 40817810000000000004",
		"11.06.2026 | 350 000,00 | RUR |  | Перевод C191106260000002 из другого банка по номеру телефона от отправителя Иван И. |  |  | 40817810000000000004",
	})
}

// A PDF of none of the banks known here — a certificate of interest paid, a
// debt's statement, another bank — is not read as a statement: nothing is
// loaded quietly from it.
func TestAPDFOfNoKnownStatementIsNotRead(t *testing.T) {
	certificate := []pdfRow{
		at(800, "200:Справка о выплаченных процентах по счёту"),
		at(700, "50:(подпись работника АО «АЛЬФА-БАНК»)"),
	}
	if _, err := fromPDFPages([][]pdfRow{certificate}); err == nil || !strings.Contains(err.Error(), "not a bank's statement") {
		t.Errorf("a certificate read as %v, want it known for no statement", err)
	}
}
