// Package table imports operations from a table a person uploads — a CSV a
// broker, a bank or a spreadsheet exported. The person says which column means
// what (Mapping, guessed from the header to start with); every row is then read
// into a journal operation or refused with the reason, and the journal is asked
// whether it takes the lot, by the same path a broker import writes through
// (operation.Service.ApplyImportDelta). Rows it wrote carry the source "csv".
package table

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
)

// MaxRows is the most rows one table may carry. A year of an active account
// is a few thousand; past this the preview would be a page nobody reads.
const MaxRows = 5000

// ErrBadTable is a table this package cannot read at all.
var ErrBadTable = errors.New("table: cannot read the table")

// Table is a CSV split into cells. Lines are numbered as a spreadsheet shows
// them, from 1, so a row can be named to the person who has the file open.
type Table struct {
	Rows []Line
}

// Line is one non-empty line of the file.
type Line struct {
	Number int
	Cells  []string
}

// Parse reads CSV text: the delimiter is whichever of ';', ',' and a tab
// splits the first lines most consistently (Russian exports use ';' because
// ',' is their decimal mark); a byte-order mark is dropped; blank lines are
// skipped.
func Parse(content string) (Table, error) {
	content = strings.TrimPrefix(content, "\uFEFF")
	if strings.TrimSpace(content) == "" {
		return Table{}, fmt.Errorf("%w: the file is empty", ErrBadTable)
	}
	r := csv.NewReader(strings.NewReader(content))
	r.Comma = delimiter(content)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	var out Table
	for {
		cells, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Table{}, fmt.Errorf("%w: %v", ErrBadTable, err)
		}
		if blank(cells) {
			continue
		}
		if len(out.Rows) == MaxRows+1 {
			return Table{}, fmt.Errorf("%w: more than %d rows; split the file", ErrBadTable, MaxRows)
		}
		line, _ := r.FieldPos(0)
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		out.Rows = append(out.Rows, Line{Number: line, Cells: cells})
	}
	return out, nil
}

// delimiter picks the separator that gives the first lines the same, largest
// number of cells.
func delimiter(content string) rune {
	head := content
	if lines := strings.SplitN(content, "\n", 6); len(lines) > 5 {
		head = strings.Join(lines[:5], "\n")
	}
	best, bestScore := ';', -1
	for _, d := range []rune{';', ',', '\t'} {
		r := csv.NewReader(bytes.NewBufferString(head))
		r.Comma = d
		r.FieldsPerRecord = -1
		r.LazyQuotes = true
		records, err := r.ReadAll()
		if err != nil || len(records) == 0 {
			continue
		}
		width, steady := len(records[0]), true
		for _, rec := range records[1:] {
			if len(rec) != width {
				steady = false
			}
		}
		score := width
		if !steady {
			score = width / 2
		}
		if width > 1 && score > bestScore {
			best, bestScore = d, score
		}
	}
	return best
}

func blank(cells []string) bool {
	for _, c := range cells {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
