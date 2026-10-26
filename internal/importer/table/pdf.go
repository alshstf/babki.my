package table

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/ledongthuc/pdf"
)

// maxPDFPages bounds a statement: half a year of an active card is thirty
// pages; past this the file is not a bank's statement.
const maxPDFPages = 300

// pdfCell is a run of text on a page and where it starts, in points from the
// page's left edge.
type pdfCell struct {
	X    float64
	Text string
}

// pdfRow is a line of a page: its height from the page's bottom and its
// runs, left to right. Blank runs are left out.
type pdfRow struct {
	Y     float64
	Cells []pdfCell
}

// text is the row's runs joined by blanks.
func (r pdfRow) text() string {
	parts := make([]string, len(r.Cells))
	for i, c := range r.Cells {
		parts[i] = c.Text
	}
	return strings.Join(parts, " ")
}

// pdfPages reads a PDF's text page by page, each page's rows top to bottom
// and each row's runs left to right, as the reader orders them.
// A file this cannot read — not a PDF, damaged, encrypted, or one the reader
// trips over — is ErrBadTable, never a panic: an upload is anyone's.
func pdfPages(data []byte) (pages [][]pdfRow, err error) {
	defer func() {
		if p := recover(); p != nil {
			pages, err = nil, fmt.Errorf("%w: the PDF cannot be read", ErrBadTable)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: the PDF cannot be read: %v", ErrBadTable, err)
	}
	n := r.NumPage()
	if n > maxPDFPages {
		return nil, fmt.Errorf("%w: more than %d pages", ErrBadTable, maxPDFPages)
	}
	for i := 1; i <= n; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		rows, err := p.GetTextByRow()
		if err != nil {
			return nil, fmt.Errorf("%w: page %d cannot be read: %v", ErrBadTable, i, err)
		}
		var page []pdfRow
		for _, row := range rows {
			out := pdfRow{Y: float64(row.Position)}
			for _, t := range row.Content {
				if s := strings.TrimSpace(t.S); s != "" {
					out.Cells = append(out.Cells, pdfCell{X: t.X, Text: s})
				}
			}
			if len(out.Cells) > 0 {
				page = append(page, out)
			}
		}
		pages = append(pages, page)
	}
	return pages, nil
}
