package export

import (
	"fmt"
	"strings"
	"time"
)

// Result is a rendered report: named columns and the rows beneath them.
type Result struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Columns     []string   `json:"columns"`
	Rows        [][]any    `json:"rows"`
	RowCount    int        `json:"row_count"`
	GeneratedAt time.Time  `json:"generated_at"`
	From        *time.Time `json:"from,omitempty"`
	To          *time.Time `json:"to,omitempty"`
}

// CSV renders a result as a spreadsheet-friendly file.
//
// Written by hand rather than with encoding/csv so the formula-injection guard
// below is unmissable: a cell beginning with =, +, - or @ is executed by Excel
// when the file is opened, and ticket subjects are attacker-controlled text.
func (res *Result) CSV() []byte {
	var b strings.Builder

	writeRow := func(cells []string) {
		for i, cell := range cells {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(csvCell(cell))
		}
		b.WriteByte('\n')
	}

	writeRow(res.Columns)
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = renderCell(v)
		}
		writeRow(cells)
	}
	return []byte(b.String())
}

func renderCell(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case time.Time:
		return value.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", value)
	}
}

// csvCell quotes a value and neutralises spreadsheet formulas.
func csvCell(s string) string {
	// A leading =, +, - or @ makes Excel and Sheets treat the cell as a formula.
	// Prefixing a single quote is the documented way to force text, and it is
	// stripped on display.
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		s = "'" + s
	}
	if strings.ContainsAny(s, ",\"\n\r") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}
