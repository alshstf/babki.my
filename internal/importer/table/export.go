package table

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"babki.my/babki/internal/family"
	"babki.my/babki/internal/instrument"
	"babki.my/babki/internal/operation"
)

// exportHeader is the header a journal is written out under: the words the
// import recognizes (see Guess), so a file written here reads back as it is.
var exportHeader = []string{"Дата", "Тип", "Бумага", "Количество", "Цена", "Сумма", "Валюта", "Комиссия", "Заметка"}

// exportWords names each type in the file. The ones a table may hold are the
// words the import knows; the rest are named for the person reading the file
// and are not read back.
var exportWords = map[operation.Type]string{
	operation.TypeBuy: "Покупка", operation.TypeSell: "Продажа", operation.TypeDeposit: "Пополнение",
	operation.TypeWithdrawal: "Вывод", operation.TypeDividend: "Дивиденд", operation.TypeCoupon: "Купон",
	operation.TypeInterest: "Проценты", operation.TypeTax: "Налог", operation.TypeFee: "Комиссия",
	operation.TypeAmortization: "Амортизация", operation.TypeRedemption: "Погашение",
	operation.TypeTransferIn: "Зачисление бумаг", operation.TypeTransferOut: "Списание бумаг",
	operation.TypeSplit: "Сплит", operation.TypeConversion: "Конвертация",
	operation.TypeExchangeIn: "Обмен (зачисление)", operation.TypeExchangeOut: "Обмен (списание)",
	operation.TypeSpinoffIn: "Выделение (зачисление)", operation.TypeSpinoffOut: "Выделение (списание)",
}

type catalogByID interface {
	ByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]instrument.Instrument, error)
}

// Export writes the account's journal as a table, oldest first: semicolons
// between cells and decimal commas, as a Russian spreadsheet expects, and a
// byte-order mark so it opens as UTF-8.
func (s *Service) Export(ctx context.Context, spaceID, accountID uuid.UUID, papers catalogByID) ([]byte, error) {
	if _, err := s.accounts.ByID(ctx, spaceID, accountID); err != nil {
		return nil, err
	}
	ops, err := s.journal.ListForEngine(ctx, spaceID, accountID)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, o := range ops {
		if o.InstrumentID != nil {
			ids = append(ids, *o.InstrumentID)
		}
	}
	byID, err := papers.ByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString("\uFEFF")
	w := csv.NewWriter(&buf)
	w.Comma = ';'
	if err := w.Write(exportHeader); err != nil {
		return nil, err
	}
	for _, o := range ops {
		paper := ""
		if o.InstrumentID != nil {
			if p, ok := byID[*o.InstrumentID]; ok {
				paper = p.ISIN
				if paper == "" {
					paper = p.Ticker
				}
			}
		}
		word := exportWords[o.Type]
		if word == "" {
			word = string(o.Type)
		}
		record := []string{
			o.OccurredOn.Format("02.01.2006"), word, paper,
			decimalCell(o.Quantity), decimalCell(o.Price),
			minorCell(o.AmountMinor), o.Currency, feeCell(o.FeeMinor), o.Note,
		}
		if err := w.Write(record); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

func decimalCell(d *decimal.Decimal) string {
	if d == nil {
		return ""
	}
	return strings.Replace(d.String(), ".", ",", 1)
}

func minorCell(minor int64) string {
	return strings.Replace(decimal.New(minor, -2).StringFixed(2), ".", ",", 1)
}

func feeCell(minor int64) string {
	if minor == 0 {
		return ""
	}
	return minorCell(minor)
}

func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	p, _ := family.PrincipalFromContext(r.Context())
	accountID, ok := pathID(w, r, "accountId")
	if !ok {
		return
	}
	body, err := h.svc.Export(r.Context(), p.SpaceID, accountID, h.papers)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="journal-%s.csv"`, accountID))
	_, _ = w.Write(body)
}
