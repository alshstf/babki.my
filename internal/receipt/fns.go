package receipt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"babki.my/babki/internal/family"
)

// ParseFNS reads the receipts out of a statement the tax service's app
// «Проверка чеков» mails (decision Р-34): a JSON file of the receipts scanned
// or sent to the owner's phone, each with the seller and the items. Its shape
// is described nowhere, and has changed over the years — the receipt itself
// sits under ticket.document.receipt, document.receipt or at the top — so any
// object carrying a fiscal drive's number, a document's number and a total is
// taken as a receipt, wherever it lies. Sums are in kopecks; the time is the
// till's, as an ISO string or Unix seconds.
func ParseFNS(data []byte) ([]Receipt, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: the file is not JSON: %v", family.ErrValidation, err)
	}
	var out []Receipt
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if r, ok := fnsReceipt(x); ok {
				out = append(out, r)
				return
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	return out, nil
}

// fnsReceipt is the receipt an object holds, if it is one: a drive's number,
// a document's number, a total and a time at least.
func fnsReceipt(m map[string]any) (Receipt, bool) {
	fn, okFN := digitsOf(m["fiscalDriveNumber"])
	fd, okFD := digitsOf(m["fiscalDocumentNumber"])
	total, okTotal := kopecks(m["totalSum"])
	at, okAt := tillTime(m["dateTime"])
	if !okFN || !okFD || !okTotal || !okAt || total <= 0 {
		return Receipt{}, false
	}
	r := Receipt{FN: fn, FD: fd, Total: total, IssuedAt: at, Kind: fnsKind(m["operationType"]), Source: SourceFNS, Items: []Item{}}
	if fp, ok := digitsOf(m["fiscalSign"]); ok {
		r.FP = &fp
	}
	r.Seller = text(m["user"], 300)
	if inn, ok := m["userInn"].(string); ok {
		if inn = strings.TrimSpace(inn); digits.MatchString(inn) && (len(inn) == 10 || len(inn) == 12) {
			r.SellerINN = &inn
		}
	}
	r.Address = text(m["retailPlaceAddress"], 500)
	if r.Address == nil {
		r.Address = text(m["retailPlace"], 500)
	}
	if items, ok := m["items"].([]any); ok {
		for _, raw := range items {
			it, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name := text(it["name"], 300)
			sum, okSum := kopecks(it["sum"])
			if name == nil || !okSum {
				continue
			}
			price, _ := kopecks(it["price"])
			r.Items = append(r.Items, Item{Name: *name, Quantity: quantity(it["quantity"]), Price: price, Sum: sum})
		}
	}
	return r, true
}

// fnsKind is the receipt's kind by its operationType: 1 a purchase, 2 its
// refund, 3 the seller paying out, 4 the refund of that; a purchase when
// it says nothing.
func fnsKind(v any) Kind {
	n, _ := number(v)
	switch n {
	case 2:
		return Refund
	case 3:
		return Payout
	case 4:
		return PayoutRefund
	}
	return Purchase
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// digitsOf is a number or a string of digits as its digits.
func digitsOf(v any) (string, bool) {
	var s string
	switch x := v.(type) {
	case json.Number:
		s = x.String()
	case string:
		s = strings.TrimSpace(x)
	default:
		return "", false
	}
	return s, digits.MatchString(s)
}

// kopecks is a whole sum in kopecks.
func kopecks(v any) (int64, bool) {
	f, ok := number(v)
	if !ok || f < 0 || f > 1e15 {
		return 0, false
	}
	return int64(math.Round(f)), true
}

// tillTime is the till's time: an ISO string, with or without seconds and a
// zone (the zone is dropped — it is the till's wall clock), or Unix seconds.
func tillTime(v any) (time.Time, bool) {
	wall := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	}
	if s, ok := v.(string); ok {
		for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", time.RFC3339} {
			if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
				return wall(t), true
			}
		}
		return time.Time{}, false
	}
	n, ok := number(v)
	if !ok || n <= 0 {
		return time.Time{}, false
	}
	return wall(time.Unix(int64(n), 0).UTC()), true
}

// quantity is an item's quantity as a decimal: «1», «0.546».
func quantity(v any) string {
	f, ok := number(v)
	if !ok {
		return "1"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// text is a trimmed string no longer than limit runes; nil when empty.
func text(v any, limit int) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return nil
	}
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit])
	}
	return &s
}
