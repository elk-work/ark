// Package output renders command results as human-readable text or stable
// JSON. Agents get --json; humans get aligned tables.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Printer renders results to one writer in one mode.
type Printer struct {
	W    io.Writer
	JSON bool
}

// JSONValue writes v as indented JSON.
func (p *Printer) JSONValue(v any) error {
	enc := json.NewEncoder(p.W)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Result renders v as JSON in JSON mode, otherwise runs human().
func (p *Printer) Result(v any, human func()) error {
	if p.JSON {
		return p.JSONValue(v)
	}
	human()
	return nil
}

// Line prints a formatted line in human mode only.
func (p *Printer) Line(format string, args ...any) {
	fmt.Fprintf(p.W, format+"\n", args...)
}

// Table prints rows with columns aligned on two spaces.
func (p *Printer) Table(header []string, rows [][]string) {
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	printRow := func(cells []string) {
		var b strings.Builder
		for i, cell := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			if i == len(cells)-1 {
				b.WriteString(cell)
			} else {
				b.WriteString(cell + strings.Repeat(" ", widths[i]-len(cell)))
			}
		}
		fmt.Fprintln(p.W, strings.TrimRight(b.String(), " "))
	}
	printRow(header)
	for _, row := range rows {
		printRow(row)
	}
}
