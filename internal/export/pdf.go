package export

import (
	"bytes"
	"fmt"
	"strings"
)

// A minimal PDF writer for tabular reports.
//
// Written by hand rather than pulled in as a dependency: what a report needs is
// a title, a header row and a grid of text in a fixed-width font, and every Go
// PDF library brings font embedding, image codecs and a layout engine to do it.
// The output uses the four standard Type 1 fonts every reader has built in, so
// nothing is embedded and the file stays a few kilobytes.
//
// Deliberately not general-purpose: it lays out one table, wraps nothing, and
// truncates a cell that will not fit. A report that needs more than that should
// be downloaded as a spreadsheet, which is why the format picker offers one.

const (
	pdfPageWidth   = 842.0 // A4 landscape, points
	pdfPageHeight  = 595.0
	pdfMargin      = 32.0
	pdfTitleSize   = 15.0
	pdfSubtleSize  = 8.5
	pdfHeaderSize  = 8.5
	pdfBodySize    = 8.0
	pdfLineHeight  = 13.0
	pdfHeaderColor = "0.12 0.31 0.47"
)

// PDF renders the result as a landscape A4 table, paginated.
func (res *Result) PDF() ([]byte, error) {
	// Column widths proportional to the widest cell in each column, so a
	// "Subject" column gets the room a "Total" column does not need.
	weights := make([]float64, len(res.Columns))
	for i, column := range res.Columns {
		widest := len(column)
		for _, row := range res.Rows {
			if i < len(row) {
				if n := len(renderCell(row[i])); n > widest {
					widest = n
				}
			}
		}
		// Clamped so one very long cell cannot squeeze every other column out.
		weights[i] = float64(clamp(widest, 6, 42))
	}

	total := 0.0
	for _, w := range weights {
		total += w
	}
	usable := pdfPageWidth - 2*pdfMargin
	widths := make([]float64, len(weights))
	for i, w := range weights {
		widths[i] = usable * (w / total)
	}

	// How many body rows fit below the heading block on each page.
	const headingBlock = 74.0
	available := float64(pdfPageHeight - pdfMargin - headingBlock - pdfMargin)
	perPage := int(available / pdfLineHeight)
	if perPage < 1 {
		perPage = 1
	}

	pages := [][][]any{}
	for start := 0; start < len(res.Rows); start += perPage {
		end := start + perPage
		if end > len(res.Rows) {
			end = len(res.Rows)
		}
		pages = append(pages, res.Rows[start:end])
	}
	if len(pages) == 0 {
		pages = [][][]any{{}} // an empty report still produces a page saying so
	}

	doc := newPDFDoc()
	for i, rows := range pages {
		doc.addPage(res.renderPage(rows, widths, i+1, len(pages)))
	}
	return doc.bytes()
}

// renderPage produces the content stream for one page.
func (res *Result) renderPage(rows [][]any, widths []float64, page, pages int) string {
	var b strings.Builder

	y := pdfPageHeight - pdfMargin - pdfTitleSize

	text := func(font string, size, x, at float64, value string) {
		fmt.Fprintf(&b, "BT /%s %.1f Tf %.1f %.1f Td (%s) Tj ET\n",
			font, size, x, at, pdfEscape(value))
	}

	text("F2", pdfTitleSize, pdfMargin, y, res.Title)
	y -= 15

	subtitle := "Generated " + res.GeneratedAt.Format("02 Jan 2006 15:04") + " UTC  ·  " +
		describeWindow(res.From, res.To) + fmt.Sprintf("  ·  %d row(s)", res.RowCount)
	fmt.Fprintf(&b, "0.35 0.35 0.35 rg\n")
	text("F1", pdfSubtleSize, pdfMargin, y, subtitle)
	fmt.Fprintf(&b, "0 0 0 rg\n")
	y -= 20

	// Header band.
	fmt.Fprintf(&b, "%s rg %.1f %.1f %.1f %.1f re f\n",
		pdfHeaderColor, pdfMargin, y-4, pdfPageWidth-2*pdfMargin, pdfLineHeight+2)
	fmt.Fprintf(&b, "1 1 1 rg\n")

	x := pdfMargin
	for i, column := range res.Columns {
		text("F2", pdfHeaderSize, x+3, y, pdfTruncate(column, widths[i]-6, pdfHeaderSize))
		x += widths[i]
	}
	fmt.Fprintf(&b, "0 0 0 rg\n")
	y -= pdfLineHeight + 4

	if len(rows) == 0 {
		fmt.Fprintf(&b, "0.45 0.45 0.45 rg\n")
		text("F1", pdfBodySize, pdfMargin+3, y, "No rows matched these filters.")
		fmt.Fprintf(&b, "0 0 0 rg\n")
	}

	for r, row := range rows {
		// Zebra striping: the alternative on a wide table is losing your place
		// halfway across a row.
		if r%2 == 1 {
			fmt.Fprintf(&b, "0.96 0.96 0.96 rg %.1f %.1f %.1f %.1f re f\n0 0 0 rg\n",
				pdfMargin, y-3, pdfPageWidth-2*pdfMargin, pdfLineHeight)
		}
		x = pdfMargin
		for i := range res.Columns {
			value := ""
			if i < len(row) {
				value = renderCell(row[i])
			}
			text("F1", pdfBodySize, x+3, y, pdfTruncate(value, widths[i]-6, pdfBodySize))
			x += widths[i]
		}
		y -= pdfLineHeight
	}

	fmt.Fprintf(&b, "0.45 0.45 0.45 rg\n")
	text("F1", pdfSubtleSize, pdfMargin, pdfMargin-6,
		fmt.Sprintf("ComplyDesk  ·  page %d of %d", page, pages))
	fmt.Fprintf(&b, "0 0 0 rg\n")

	return b.String()
}

// --- the container ----------------------------------------------------------

type pdfDoc struct{ pages []string }

func newPDFDoc() *pdfDoc { return &pdfDoc{} }

func (d *pdfDoc) addPage(content string) { d.pages = append(d.pages, content) }

// bytes assembles the object table and the cross-reference table.
//
// Object numbering: 1 catalogue, 2 page tree, 3 Helvetica, 4 Helvetica-Bold,
// then for each page a page object followed by its content stream.
func (d *pdfDoc) bytes() ([]byte, error) {
	var out bytes.Buffer
	offsets := []int{}

	out.WriteString("%PDF-1.4\n")
	// A binary comment marks the file as binary for transfer agents.
	out.Write([]byte{'%', 0xE2, 0xE3, 0xCF, 0xD3, '\n'})

	object := func(body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}

	firstPageObj := 5
	kids := make([]string, 0, len(d.pages))
	for i := range d.pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", firstPageObj+i*2))
	}

	object(`<< /Type /Catalog /Pages 2 0 R >>`)
	object(fmt.Sprintf(`<< /Type /Pages /Kids [%s] /Count %d >>`,
		strings.Join(kids, " "), len(d.pages)))
	object(`<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>`)
	object(`<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>`)

	for i, content := range d.pages {
		contentObj := firstPageObj + i*2 + 1
		object(fmt.Sprintf(
			`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.0f %.0f] `+
				`/Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>`,
			pdfPageWidth, pdfPageHeight, contentObj))
		object(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(offsets)+1, xref)

	return out.Bytes(), nil
}

// pdfEscape makes a string safe inside a PDF literal string.
//
// Backslash and both parentheses are the delimiters, and an unbalanced one from
// a ticket subject would corrupt the whole stream. Non-Latin-1 runes are
// replaced rather than dropped, because the standard fonts cannot show them and
// a silent gap reads as missing data.
func pdfEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '(' || r == ')':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 32:
			// Control characters have no glyph; skip them.
		case r > 255:
			b.WriteByte('?')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// pdfTruncate shortens a cell to fit its column.
//
// Helvetica averages about 0.5 em per character at these sizes; close enough
// for a fixed-width grid, and erring towards truncating early keeps columns
// from colliding.
func pdfTruncate(s string, width, size float64) string {
	max := int(width / (size * 0.5))
	if max < 1 {
		max = 1
	}
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
