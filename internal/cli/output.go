package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Formatter renders a command's result.
//
// There are two implementations. The human one buffers everything and emits an
// aligned key/value block or a table on Flush. The JSON one accumulates an
// ordered object and writes it, and only it, to stdout — every advisory line
// goes to stderr instead, so that `gflex --json info | jq` never sees anything
// but the object.
//
// Keys passed to KV and Table are the wire field names from SPEC.md §8 and are
// used verbatim in JSON output; the label and display strings are for humans
// only. Values are always in wire units: no command converts millivolts to
// volts anywhere except in a display string.
type Formatter interface {
	// JSON reports whether machine-readable output was requested. Commands
	// that must branch on the output mode (streaming ones, mostly) use this;
	// everything else should just call KV and Table.
	JSON() bool
	// KV records one result field.
	KV(key, label string, value any, display string)
	// Table records a tabular block. items is what JSON stores under key;
	// headers and rows are the human rendering.
	Table(key, title string, items any, headers []string, rows [][]string)
	// Document replaces the entire JSON result object with v. Human output is
	// unaffected, so a command that calls Document must also describe itself
	// through KV or Table.
	Document(v any)
	// Note records a line of result prose: stdout in human mode, stderr in
	// JSON mode.
	Note(format string, args ...any)
	// Diag writes a diagnostic. It always goes to stderr, in both modes.
	Diag(format string, args ...any)
	// Flush emits the accumulated result.
	Flush() error
}

// newFormatter builds the formatter selected by --json.
func newFormatter(asJSON bool, stdout, stderr io.Writer) Formatter {
	if asJSON {
		return &jsonFormatter{doc: newOrderedMap(), out: stdout, err: stderr}
	}
	return &humanFormatter{out: stdout, err: stderr}
}

// ---------------------------------------------------------------------------
// Human formatter
// ---------------------------------------------------------------------------

type entryKind int

const (
	entryKV entryKind = iota
	entryTable
	entryNote
)

type entry struct {
	kind    entryKind
	label   string
	display string
	title   string
	headers []string
	rows    [][]string
	text    string
}

// humanFormatter buffers entries so that key/value runs can be column-aligned
// against each other while preserving the order the command produced them in.
type humanFormatter struct {
	out     io.Writer
	err     io.Writer
	entries []entry
}

func (h *humanFormatter) JSON() bool { return false }

// KV records a field. An entry with neither a label nor a display string is
// JSON-only: it exists so a paired value (the second half of a range, a raw
// payload beside its decoded form) reaches the object without adding a blank
// row to the human block.
func (h *humanFormatter) KV(_, label string, _ any, display string) {
	if label == "" && display == "" {
		return
	}
	h.entries = append(h.entries, entry{kind: entryKV, label: label, display: display})
}

func (h *humanFormatter) Table(_, title string, _ any, headers []string, rows [][]string) {
	h.entries = append(h.entries, entry{kind: entryTable, title: title, headers: headers, rows: rows})
}

func (h *humanFormatter) Document(any) {}

func (h *humanFormatter) Note(format string, args ...any) {
	h.entries = append(h.entries, entry{kind: entryNote, text: fmt.Sprintf(format, args...)})
}

func (h *humanFormatter) Diag(format string, args ...any) {
	fmt.Fprintln(h.err, fmt.Sprintf(format, args...))
}

func (h *humanFormatter) Flush() error {
	var buf bytes.Buffer
	for i := 0; i < len(h.entries); i++ {
		e := h.entries[i]
		switch e.kind {
		case entryKV:
			// Align every key/value entry in this uninterrupted run against
			// the widest label in the run, not against the whole output.
			j := i
			width := 0
			for j < len(h.entries) && h.entries[j].kind == entryKV {
				// +1 for the colon appended when the row is printed.
				if n := len(h.entries[j].label) + 1; n > width {
					width = n
				}
				j++
			}
			for _, kv := range h.entries[i:j] {
				fmt.Fprintf(&buf, "%-*s  %s\n", width, kv.label+":", kv.display)
			}
			i = j - 1
		case entryTable:
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			if e.title != "" {
				fmt.Fprintf(&buf, "%s\n", e.title)
			}
			writeTable(&buf, e.headers, e.rows)
		case entryNote:
			fmt.Fprintf(&buf, "%s\n", e.text)
		}
	}
	_, err := h.out.Write(buf.Bytes())
	return err
}

// writeTable renders headers and rows as a space-padded table. Wide content is
// not truncated: a terminal wrapping a long PDO line is better than silently
// hiding a capability.
func writeTable(w io.Writer, headers []string, rows [][]string) {
	if len(headers) == 0 && len(rows) == 0 {
		return
	}
	n := len(headers)
	for _, r := range rows {
		if len(r) > n {
			n = len(r)
		}
	}
	width := make([]int, n)
	for i, hcell := range headers {
		width[i] = len(hcell)
	}
	for _, r := range rows {
		for i, c := range r {
			if len(c) > width[i] {
				width[i] = len(c)
			}
		}
	}
	writeRow := func(cells []string) {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			var c string
			if i < len(cells) {
				c = cells[i]
			}
			if i == n-1 {
				sb.WriteString(c)
			} else {
				fmt.Fprintf(&sb, "%-*s  ", width[i], c)
			}
		}
		fmt.Fprintln(w, strings.TrimRight(sb.String(), " "))
	}
	if len(headers) > 0 {
		writeRow(headers)
		rule := make([]string, n)
		for i := range rule {
			rule[i] = strings.Repeat("-", width[i])
		}
		writeRow(rule)
	}
	for _, r := range rows {
		writeRow(r)
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "(none)")
	}
}

// ---------------------------------------------------------------------------
// JSON formatter
// ---------------------------------------------------------------------------

// jsonFormatter accumulates a single object. Nothing else is ever written to
// stdout, so the output is always parseable in full.
type jsonFormatter struct {
	doc *orderedMap
	// document, when non-nil, replaces doc entirely. It carries a struct such
	// as *proto.DeviceInfo or *pdo.Log so the JSON field names come straight
	// from the shared model rather than being retyped here.
	document any
	hasDoc   bool
	out      io.Writer
	err      io.Writer
}

func (j *jsonFormatter) JSON() bool { return true }

func (j *jsonFormatter) KV(key, _ string, value any, _ string) {
	if key != "" {
		j.doc.set(key, value)
	}
}

func (j *jsonFormatter) Table(key, _ string, items any, _ []string, _ [][]string) {
	if key != "" {
		j.doc.set(key, items)
	}
}

func (j *jsonFormatter) Document(v any) {
	j.document, j.hasDoc = v, true
}

// Note goes to stderr in JSON mode: prose is not part of the object, and
// letting it reach stdout would corrupt the document.
func (j *jsonFormatter) Note(format string, args ...any) {
	fmt.Fprintln(j.err, fmt.Sprintf(format, args...))
}

func (j *jsonFormatter) Diag(format string, args ...any) {
	fmt.Fprintln(j.err, fmt.Sprintf(format, args...))
}

func (j *jsonFormatter) Flush() error {
	var v any = j.doc
	if j.hasDoc {
		v = j.document
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding JSON output: %w", err)
	}
	b = append(b, '\n')
	_, err = j.out.Write(b)
	return err
}

// ---------------------------------------------------------------------------
// Ordered object
// ---------------------------------------------------------------------------

// orderedMap is a JSON object that marshals its keys in insertion order.
// encoding/json sorts map keys alphabetically, which would scatter related
// fields (voltage_mv next to vtolerance_sag_per_ma) and make diffs noisy.
type orderedMap struct {
	keys []string
	vals map[string]any
}

func newOrderedMap() *orderedMap {
	return &orderedMap{vals: make(map[string]any)}
}

func (m *orderedMap) set(k string, v any) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

// MarshalJSON implements json.Marshaler.
func (m *orderedMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(m.vals[k])
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", k, err)
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
