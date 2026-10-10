package receipt

import (
	"errors"
	"testing"
	"time"

	"babki.my/babki/internal/family"
)

// The statement «Проверка чеков» mails, as it was described for 1C's
// loaders: receipts under ticket.document.receipt, sums in kopecks, the
// seller's INN padded with spaces. Made up, not a family's.
const statement = `[
  {"_id": "1", "createdAt": "2026-10-10T08:00:00+00:00", "ticket": {"document": {"receipt": {
    "dateTime": "2026-10-09T19:15:00", "fiscalDriveNumber": "7380440700123456", "fiscalDocumentNumber": 51243,
    "fiscalSign": 1234567890, "operationType": 1, "totalSum": 234090,
    "user": "ООО  \"АГРОТОРГ\" ", "userInn": "7825706086  ", "retailPlaceAddress": "г. Москва, ул. Ленина, 5",
    "items": [
      {"name": "Молоко 3,2% 1л", "price": 8999, "quantity": 1, "sum": 8999},
      {"name": "Порошок стиральный 3кг", "price": 54999, "quantity": 1, "sum": 54999},
      {"name": "Бананы", "price": 13999, "quantity": 1.2, "sum": 16799}
    ]}}}},
  {"_id": "2", "ticket": {"document": {"receipt": {
    "dateTime": 1791580800, "fiscalDriveNumber": 9282440300810826, "fiscalDocumentNumber": "777",
    "operationType": 2, "totalSum": 50000, "items": []}}}},
  {"_id": "3", "ticket": {"document": {"receipt": {"dateTime": "2026-10-09T10:00:00", "totalSum": 100}}}}
]`

func TestTheTaxServiceStatementIsRead(t *testing.T) {
	got, err := ParseFNS([]byte(statement))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("receipts = %d, want 2: the third has no numbers", len(got))
	}
	r := got[0]
	if r.FN != "7380440700123456" || r.FD != "51243" || r.FP == nil || *r.FP != "1234567890" || r.Kind != Purchase ||
		r.Total != 234090 || !r.IssuedAt.Equal(time.Date(2026, 10, 9, 19, 15, 0, 0, time.UTC)) || r.Source != SourceFNS {
		t.Errorf("first = %+v", r)
	}
	if r.Seller == nil || *r.Seller != `ООО "АГРОТОРГ"` || r.SellerINN == nil || *r.SellerINN != "7825706086" ||
		r.Address == nil || *r.Address != "г. Москва, ул. Ленина, 5" {
		t.Errorf("seller = %v, %v, %v", r.Seller, r.SellerINN, r.Address)
	}
	if len(r.Items) != 3 || r.Items[1].Name != "Порошок стиральный 3кг" || r.Items[1].Sum != 54999 || r.Items[2].Quantity != "1.2" {
		t.Errorf("items = %+v", r.Items)
	}
	// Unix seconds, a number for the drive, a string for the document.
	if r := got[1]; r.FN != "9282440300810826" || r.FD != "777" || r.Kind != Refund || r.IssuedAt.IsZero() || r.Seller != nil {
		t.Errorf("second = %+v", r)
	}

	// Wrapped otherwise, it is found all the same.
	got, err = ParseFNS([]byte(`{"document": {"receipt": {"dateTime": "2026-10-09T19:15", "fiscalDriveNumber": "1", "fiscalDocumentNumber": 2, "totalSum": 300}}}`))
	if err != nil || len(got) != 1 || got[0].Total != 300 {
		t.Errorf("document.receipt = %+v, %v", got, err)
	}
	if got, err := ParseFNS([]byte(`{"something": "else"}`)); err != nil || len(got) != 0 {
		t.Errorf("no receipts = %+v, %v", got, err)
	}
	if _, err := ParseFNS([]byte(`not json`)); !errors.Is(err, family.ErrValidation) {
		t.Errorf("not JSON = %v, want a validation error", err)
	}
}
