// Package export renders a table of results as a downloadable file.
//
// It lives apart from the packages that produce those tables — reports,
// the ticket list — because both need the same three formats and neither
// should depend on the other. Nothing here knows what the rows mean.
package export

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xuri/excelize/v2"
)

// Export formats a report offers.
//
// CSV opens anywhere and is the safe default. XLSX is what a finance or HR team
// actually works in — typed cells, a frozen header, sensible widths. PDF is for
// circulating a result that should not be edited.
const (
	FormatCSV  = "csv"
	FormatXLSX = "xlsx"
	FormatPDF  = "pdf"
)

// Formats is the list the UI offers, in the order it offers them.
var Formats = []string{FormatCSV, FormatXLSX, FormatPDF}

// ValidFormat reports whether a requested format is one we render.
func ValidFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatCSV, FormatXLSX, FormatPDF:
		return true
	}
	return false
}

// ContentType is the MIME type a rendered report is served with.
func ContentType(format string) string {
	switch format {
	case FormatXLSX:
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case FormatPDF:
		return "application/pdf"
	default:
		return "text/csv; charset=utf-8"
	}
}

// Render turns a result into a downloadable file in the requested format.
func (res *Result) Render(format string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case FormatXLSX:
		return res.XLSX()
	case FormatPDF:
		return res.PDF()
	default:
		return res.CSV(), nil
	}
}

// XLSX renders the result as a real spreadsheet.
//
// Numbers are written as numbers rather than as text, so the recipient can sum
// a column without retyping it — which is the only reason to prefer this format
// over CSV in the first place.
func (res *Result) XLSX() ([]byte, error) {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	const sheet = "Report"
	index, err := f.NewSheet(sheet)
	if err != nil {
		return nil, fmt.Errorf("creating sheet: %w", err)
	}
	f.SetActiveSheet(index)
	_ = f.DeleteSheet("Sheet1")

	header, err := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"1F4E79"}},
		Alignment: &excelize.Alignment{
			Vertical: "center", WrapText: true,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating header style: %w", err)
	}

	// Row 1: what this is and when it was produced. A spreadsheet that arrives
	// by email without that context cannot be trusted a week later.
	_ = f.SetCellStr(sheet, "A1", res.Title)
	titleStyle, _ := f.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true, Size: 14}})
	_ = f.SetCellStyle(sheet, "A1", "A1", titleStyle)

	subtitle := "Generated " + res.GeneratedAt.Format("02 Jan 2006 15:04") + " UTC"
	if res.From != nil || res.To != nil {
		subtitle += "  ·  " + describeWindow(res.From, res.To)
	}
	_ = f.SetCellStr(sheet, "A2", subtitle)

	const headerRow = 4
	for i, column := range res.Columns {
		cell, err := excelize.CoordinatesToCellName(i+1, headerRow)
		if err != nil {
			return nil, fmt.Errorf("addressing header cell: %w", err)
		}
		_ = f.SetCellStr(sheet, cell, column)
		_ = f.SetCellStyle(sheet, cell, cell, header)
	}

	for r, row := range res.Rows {
		for c, value := range row {
			cell, err := excelize.CoordinatesToCellName(c+1, headerRow+1+r)
			if err != nil {
				return nil, fmt.Errorf("addressing cell: %w", err)
			}
			if err := writeTyped(f, sheet, cell, value); err != nil {
				return nil, err
			}
		}
	}

	// Column widths from the widest value, so nothing arrives as ####.
	for i, column := range res.Columns {
		width := len(column)
		for _, row := range res.Rows {
			if i < len(row) {
				if n := len(renderCell(row[i])); n > width {
					width = n
				}
			}
		}
		name, err := excelize.ColumnNumberToName(i + 1)
		if err != nil {
			continue
		}
		_ = f.SetColWidth(sheet, name, name, float64(clamp(width+2, 10, 60)))
	}

	// Freeze the header and switch on the filter dropdowns, because the first
	// thing anybody does with a report is sort and filter it.
	if len(res.Columns) > 0 {
		_ = f.SetPanes(sheet, &excelize.Panes{
			Freeze: true, Split: false, YSplit: headerRow,
			TopLeftCell: "A" + strconv.Itoa(headerRow+1), ActivePane: "bottomLeft",
		})
		last, err := excelize.ColumnNumberToName(len(res.Columns))
		if err == nil {
			_ = f.AutoFilter(sheet, fmt.Sprintf("A%d:%s%d", headerRow, last, headerRow+len(res.Rows)), nil)
		}
	}

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, fmt.Errorf("writing spreadsheet: %w", err)
	}
	return buf.Bytes(), nil
}

// writeTyped puts a value in a cell as its own type, so numbers stay numbers.
func writeTyped(f *excelize.File, sheet, cell string, value any) error {
	switch v := value.(type) {
	case nil:
		return nil
	case int64:
		return f.SetCellInt(sheet, cell, v)
	case int:
		return f.SetCellInt(sheet, cell, int64(v))
	case float64:
		return f.SetCellFloat(sheet, cell, v, -1, 64)
	case time.Time:
		return f.SetCellStr(sheet, cell, v.UTC().Format("2006-01-02 15:04"))
	case []byte:
		return f.SetCellStr(sheet, cell, string(v))
	case string:
		// The database returns most aggregates as strings. A value that is
		// genuinely numeric is written as a number so the column can be summed.
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return f.SetCellInt(sheet, cell, n)
		}
		if x, err := strconv.ParseFloat(v, 64); err == nil {
			return f.SetCellFloat(sheet, cell, x, -1, 64)
		}
		return f.SetCellStr(sheet, cell, v)
	default:
		return f.SetCellStr(sheet, cell, renderCell(value))
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func describeWindow(from, to *time.Time) string {
	switch {
	case from != nil && to != nil:
		return from.Format("02 Jan 2006") + " to " + to.Format("02 Jan 2006")
	case from != nil:
		return "from " + from.Format("02 Jan 2006")
	case to != nil:
		return "up to " + to.Format("02 Jan 2006")
	default:
		return "all time"
	}
}
