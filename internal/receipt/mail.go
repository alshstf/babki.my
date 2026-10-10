package receipt

import (
	"bytes"
	"html"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // the charsets OFD letters come in: koi8-r, windows-1251
)

// ParseMail reads the receipts out of a letter (decision Р-35): an electronic
// receipt an OFD or a shop mailed. Each operator draws its letter its own way,
// so what is read is what all of them carry: the receipt's QR line
// (t=…&s=…&fn=…&i=…&fp=…&n=…, often in the «check the receipt» link), or else
// the fiscal numbers, the total and the time in the text. The seller and the
// items come with the tax service's statement (decision Р-34); a letter adds
// only the receipt.
func ParseMail(raw []byte) ([]Receipt, error) {
	texts, err := mailTexts(raw)
	if err != nil {
		return nil, err
	}
	all := strings.Join(texts, "\n")
	if found := qrLines(all); len(found) > 0 {
		return found, nil
	}
	if r, ok := fiscalText(all); ok {
		return []Receipt{r}, nil
	}
	return nil, nil
}

// mailTexts are the letter's text parts, HTML ones read as text with their
// links kept, each in its own charset.
func mailTexts(raw []byte) ([]string, error) {
	entity, err := message.Read(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, err
	}
	var out []string
	var walk func(e *message.Entity)
	walk = func(e *message.Entity) {
		if mr := e.MultipartReader(); mr != nil {
			for {
				part, err := mr.NextPart()
				if err != nil {
					return
				}
				walk(part)
			}
		}
		kind, _, _ := e.Header.ContentType()
		if kind != "text/plain" && kind != "text/html" {
			return
		}
		body, err := io.ReadAll(io.LimitReader(e.Body, 2<<20))
		if err != nil && len(body) == 0 {
			return
		}
		text := string(body)
		if kind == "text/html" {
			text = htmlText(text)
		}
		out = append(out, text)
	}
	walk(entity)
	return out, nil
}

var (
	tags   = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<[^>]+>`)
	hrefRe = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
	blank  = regexp.MustCompile(`[ \t\x{00A0}]+`)
)

// htmlText is an HTML part as text: its links first (the QR line hides in
// one), then the words without the tags.
func htmlText(s string) string {
	var links []string
	for _, m := range hrefRe.FindAllStringSubmatch(s, -1) {
		links = append(links, html.UnescapeString(m[1]))
	}
	s = tags.ReplaceAllString(s, "\n")
	s = html.UnescapeString(s)
	s = blank.ReplaceAllString(s, " ")
	return strings.Join(links, "\n") + "\n" + s
}

var qrParam = regexp.MustCompile(`(?:^|[?&;\s/])(t|s|fn|i|fp|n)=([0-9T.,]+)`)

// qrLines are the receipts whose QR line the text holds, each once.
func qrLines(text string) []Receipt {
	var out []Receipt
	seen := map[string]bool{}
	for _, at := range regexp.MustCompile(`fn=\d`).FindAllStringIndex(text, -1) {
		from, to := max(at[0]-160, 0), min(at[1]+200, len(text))
		params := map[string]string{}
		for _, m := range qrParam.FindAllStringSubmatch(text[from:to], -1) {
			if _, ok := params[m[1]]; !ok {
				params[m[1]] = m[2]
			}
		}
		r, ok := qrReceipt(params)
		if !ok || seen[r.FN+"/"+r.FD] {
			continue
		}
		seen[r.FN+"/"+r.FD] = true
		out = append(out, r)
	}
	return out
}

// qrReceipt is the receipt of a QR line's parameters, as the receipt dialog
// reads them.
func qrReceipt(p map[string]string) (Receipt, bool) {
	when, err := time.Parse("20060102T1504", p["t"][:min(len(p["t"]), 13)])
	if err != nil || !digits.MatchString(p["fn"]) || !digits.MatchString(p["i"]) {
		return Receipt{}, false
	}
	total, ok := rubles(p["s"])
	if !ok || total <= 0 {
		return Receipt{}, false
	}
	r := Receipt{FN: p["fn"], FD: p["i"], Total: total, IssuedAt: when, Kind: qrKind(p["n"]), Source: SourceMail, Items: []Item{}}
	if fp := p["fp"]; digits.MatchString(fp) {
		r.FP = &fp
	}
	return r, true
}

func qrKind(n string) Kind {
	switch n {
	case "2":
		return Refund
	case "3":
		return Payout
	case "4":
		return PayoutRefund
	}
	return Purchase
}

// rubles is «1234.50» or «1 234,50» in kopecks.
func rubles(s string) (int64, bool) {
	s = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), " ", ""), ",", ".")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f > 1e13 {
		return 0, false
	}
	return int64(f*100 + 0.5), true
}

var (
	fnText    = regexp.MustCompile(`(?i)ФН[^0-9\n]{0,20}(\d{16})`)
	fdText    = regexp.MustCompile(`(?i)ФД[^0-9\n]{0,20}(\d{1,10})`)
	fpText    = regexp.MustCompile(`(?i)ФПД?[^0-9\n]{0,20}(\d{6,20})`)
	totalText = regexp.MustCompile(`(?i)ИТОГ[^0-9\n]{0,40}(\d[\d \x{00A0}]*[.,]\d{2})`)
	whenText  = regexp.MustCompile(`(\d{2})\.(\d{2})\.(\d{2}|\d{4})\s+(\d{2}):(\d{2})`)
)

// fiscalText is the receipt the letter's text names by its fiscal numbers,
// total and time, when it names all of them.
func fiscalText(text string) (Receipt, bool) {
	fn, fd, total, when := fnText.FindStringSubmatch(text), fdText.FindStringSubmatch(text), totalText.FindStringSubmatch(text), whenText.FindStringSubmatch(text)
	if fn == nil || fd == nil || total == nil || when == nil {
		return Receipt{}, false
	}
	sum, ok := rubles(strings.ReplaceAll(total[1], " ", ""))
	if !ok || sum <= 0 {
		return Receipt{}, false
	}
	year := when[3]
	if len(year) == 2 {
		year = "20" + year
	}
	at, err := time.Parse("2006-01-02 15:04", year+"-"+when[2]+"-"+when[1]+" "+when[4]+":"+when[5])
	if err != nil {
		return Receipt{}, false
	}
	kind := Purchase
	if strings.Contains(strings.ToLower(text), "возврат прихода") {
		kind = Refund
	}
	r := Receipt{FN: fn[1], FD: fd[1], Total: sum, IssuedAt: at, Kind: kind, Source: SourceMail, Items: []Item{}}
	if fp := fpText.FindStringSubmatch(text); fp != nil {
		r.FP = &fp[1]
	}
	return r, true
}
