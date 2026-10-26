package table

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// tinyPDF is a one-page PDF of the runs given, each «text» drawn at x, y in
// Helvetica — the least a reader must make sense of.
func tinyPDF(runs ...struct {
	x, y int
	text string
}) []byte {
	var content bytes.Buffer
	for _, r := range runs {
		fmt.Fprintf(&content, "BT /F1 10 Tf 1 0 0 1 %d %d Tm (%s) Tj ET\n", r.x, r.y, r.text)
	}
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", content.Len(), content.String()),
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

// A page reads as rows top to bottom, each row's runs left to right, blank
// ones left out — whatever order the PDF drew them in.
func TestAPDFPageReadsAsRowsOfRuns(t *testing.T) {
	data := tinyPDF(
		struct {
			x, y int
			text string
		}{40, 650, "Total"},
		struct {
			x, y int
			text string
		}{300, 700, "-1 890,00"},
		struct {
			x, y int
			text string
		}{40, 700, "24.04.2026"},
		struct {
			x, y int
			text string
		}{40, 600, " "},
	)
	pages, err := pdfPages(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || len(pages[0]) != 2 {
		t.Fatalf("pages = %+v, want one page of two rows", pages)
	}
	first, second := pages[0][0], pages[0][1]
	if first.text() != "24.04.2026 -1 890,00" || first.Cells[0].X != 40 || first.Cells[1].X != 300 {
		t.Errorf("first row = %+v, want the day at 40 and the amount at 300", first)
	}
	if second.text() != "Total" || second.Y >= first.Y {
		t.Errorf("second row = %+v, want «Total» below the first", second)
	}
}

// An upload is anyone's: a file that is not a PDF, or one cut short, is a
// table that cannot be read — never a crash of the server.
func TestABrokenPDFIsUnreadableNotACrash(t *testing.T) {
	whole := tinyPDF(struct {
		x, y int
		text string
	}{40, 700, "x"})
	// The reader panics on this one, found by fuzzing it: the object the
	// root names points into a run of zeros.
	head := "%PDF-1.0\n" + strings.Repeat("0", 428)
	trap := head + "xref 0 6 0 0 f 1 0 n 0 0 n 0 0 n 0 0 n 0 0 n trailer<</Size 2/Root 1 0 R>>\nstartxref\n" +
		strconv.Itoa(len(head)) + "%%EOF"
	for name, data := range map[string][]byte{
		"not a PDF":         []byte("Дата;Сумма\n01.10.2026;100\n"),
		"cut in half":       whole[:len(whole)/2],
		"no xref":           bytes.ReplaceAll(whole, []byte("xref"), []byte("xxxx")),
		"root into garbage": []byte(trap),
	} {
		if _, err := pdfPages(data); !errors.Is(err, ErrBadTable) {
			t.Errorf("%s: err = %v, want ErrBadTable", name, err)
		}
	}
	if _, err := Read("bm90IGEgUERG", FormatPDF); !errors.Is(err, ErrBadTable) {
		t.Errorf("a PDF upload that is not one: err = %v, want ErrBadTable", err)
	}
}
