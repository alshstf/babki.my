package table

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/xuri/excelize/v2"
)

// Format is how an uploaded file travels: as text — a CSV, or a tracker's
// JSON export — or as an Excel workbook, in base64.
type Format string

const (
	FormatText Format = "text"
	FormatXLSX Format = "xlsx"
)

// Read reads an uploaded file into a table, and an export of another tracker
// into the columns this package reads (see recognize).
func Read(content string, format Format) (Table, error) {
	var (
		t   Table
		err error
	)
	switch format {
	case FormatXLSX:
		data, decodeErr := base64.StdEncoding.DecodeString(content)
		if decodeErr != nil {
			return Table{}, fmt.Errorf("%w: the workbook is not base64", ErrBadTable)
		}
		t, err = ReadWorkbook(data)
	case FormatText, "":
		if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(content, "\uFEFF")), "{") {
			t, err = readGhostfolioJSON(content)
		} else {
			t, err = Parse(content)
		}
	default:
		return Table{}, fmt.Errorf("%w: unknown file format %q", ErrBadTable, format)
	}
	if err != nil {
		return Table{}, err
	}
	return recognize(t), nil
}

// maxUnzipped bounds what a workbook may unpack to: a few thousand rows are
// a few megabytes; a file that claims more is a zip bomb or not a table.
const maxUnzipped = 64 << 20

// ReadWorkbook reads an Excel workbook (.xlsx): its first visible sheet with
// anything on it, each cell as the file keeps it — a number as its value,
// not rounded or grouped as the sheet shows it, and a date as YYYY-MM-DD.
// Rows are numbered as Excel numbers them; blank ones are skipped.
func ReadWorkbook(data []byte) (Table, error) {
	f, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{
		UnzipSizeLimit: maxUnzipped, UnzipXMLSizeLimit: maxUnzipped,
	})
	if err != nil {
		return Table{}, fmt.Errorf("%w: not an Excel workbook (.xlsx): %v", ErrBadTable, err)
	}
	defer func() { _ = f.Close() }()
	props, err := f.GetWorkbookProps()
	if err != nil {
		return Table{}, fmt.Errorf("%w: %v", ErrBadTable, err)
	}
	w := workbook{f: f, date1904: props.Date1904 != nil && *props.Date1904, dates: map[int]bool{}}
	for _, sheet := range f.GetSheetList() {
		if visible, err := f.GetSheetVisible(sheet); err != nil || !visible {
			continue
		}
		t, err := w.sheet(sheet)
		if err != nil {
			return Table{}, err
		}
		if len(t.Rows) > 0 {
			return t, nil
		}
	}
	return Table{}, fmt.Errorf("%w: the workbook is empty", ErrBadTable)
}

type workbook struct {
	f        *excelize.File
	date1904 bool
	dates    map[int]bool // style → whether it shows a date
}

func (w workbook) sheet(name string) (Table, error) {
	rows, err := w.f.GetRows(name, excelize.Options{RawCellValue: true})
	if err != nil {
		return Table{}, fmt.Errorf("%w: %v", ErrBadTable, err)
	}
	var out Table
	for i, raw := range rows {
		if blank(raw) {
			continue
		}
		if len(out.Rows) == MaxRows+1 {
			return Table{}, fmt.Errorf("%w: more than %d rows; split the file", ErrBadTable, MaxRows)
		}
		cells := make([]string, len(raw))
		for j, v := range raw {
			if cells[j], err = w.cell(name, j+1, i+1, strings.TrimSpace(v)); err != nil {
				return Table{}, err
			}
		}
		out.Rows = append(out.Rows, Line{Number: i + 1, Cells: cells})
	}
	return out, nil
}

// cell is a cell's text: a number cell's value written out plainly, as a
// date when its format shows a date; any other cell as it is.
func (w workbook) cell(sheet string, col, row int, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	ref, err := excelize.CoordinatesToCellName(col, row)
	if err != nil {
		return "", err
	}
	typ, err := w.f.GetCellType(sheet, ref)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadTable, err)
	}
	if typ != excelize.CellTypeUnset && typ != excelize.CellTypeNumber {
		return raw, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return raw, nil
	}
	style, err := w.f.GetCellStyle(sheet, ref)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadTable, err)
	}
	isDate, seen := w.dates[style]
	if !seen {
		s, err := w.f.GetStyle(style)
		isDate = err == nil && showsDate(s)
		w.dates[style] = isDate
	}
	if isDate {
		d, err := excelize.ExcelDateToTime(v, w.date1904)
		if err != nil {
			return raw, nil
		}
		if d.Equal(d.Truncate(24 * time.Hour)) {
			return d.Format(time.DateOnly), nil
		}
		return d.Format(time.DateTime), nil
	}
	// Excel keeps 15 significant digits; past them is the binary float's
	// noise (0.1 + 0.2 kept as 0.30000000000000004).
	n, err := decimal.NewFromString(strconv.FormatFloat(v, 'g', 15, 64))
	if err != nil {
		return raw, nil
	}
	return n.String(), nil
}

// showsDate says whether a number format shows a date: one of Excel's own
// date formats, or a format of the file's own with a day or a year in it
// once quoted text, escapes and [brackets] are set aside.
func showsDate(s *excelize.Style) bool {
	if s == nil {
		return false
	}
	if s.CustomNumFmt == nil {
		n := s.NumFmt
		return (n >= 14 && n <= 17) || n == 22 || (n >= 27 && n <= 36) || (n >= 50 && n <= 58)
	}
	var code strings.Builder
	quoted, bracket := false, false
	runes := []rune(*s.CustomNumFmt)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quoted:
			quoted = r != '"'
		case bracket:
			bracket = r != ']'
		case r == '"':
			quoted = true
		case r == '[':
			bracket = true
		case r == '\\' || r == '_' || r == '*':
			i++ // the next character is shown or padded with, not a code
		default:
			code.WriteRune(r)
		}
	}
	c := strings.ToLower(code.String())
	return strings.ContainsAny(c, "dy")
}
