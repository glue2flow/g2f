package nodes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/glue2flow/g2f/internal/expression"
)

type csvNode struct{}

const maxCSVRows = 100000

func (csvNode) Definition() Definition {
	show := func(ops ...string) map[string][]string { return map[string][]string{"operation": ops} }
	return dataDef("data.csv", "CSV", "Read CSV text into items (one per row), or turn items into a CSV file, "+
		"e.g. to attach it to an e-mail.", []string{"main"},
		[]Param{
			{Name: "operation", Label: "Operation", Type: ParamSelect, Default: "parse", NoExpression: true, Options: []Option{
				{"CSV text → items", "parse"}, {"Items → CSV file", "generate"},
			}},
			{Name: "text", Label: "CSV text", Type: ParamText, Required: true, Default: "{{ json.body }}", ShowIf: show("parse"),
				Description: "Base64 content (e.g. an attachment's content) is accepted as well."},
			{Name: "header", Label: "First row has column names", Type: ParamBoolean, Default: true, NoExpression: true},
			{Name: "delimiter", Label: "Delimiter", Type: ParamSelect, Default: "auto", NoExpression: true, Options: []Option{
				{"Detect (, ; tab |)", "auto"}, {"Comma", ","}, {"Semicolon", ";"}, {"Tab", "tab"}, {"Pipe", "|"},
			}},
			{Name: "columns", Label: "Columns", NoExpression: true, ShowIf: show("generate"), Placeholder: "id, name, email",
				Description: "Comma separated fields (nested: customer.name). Default: all fields of the items."},
			{Name: "filename", Label: "File name", Default: "export.csv", ShowIf: show("generate")},
		},
		&OutputSchema{Fields: fields(
			"csv:string:Items → CSV: the CSV text",
			"content:string:Items → CSV: base64 content, for e-mail attachments",
			"filename:string",
			"contentType:string",
			"rows:number",
		)})
}

func (csvNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	op, _ := ec.Raw("operation").(string)
	header := true
	if h, ok := ec.Raw("header").(bool); ok {
		header = h
	}
	delim, _ := ec.Raw("delimiter").(string)
	if op == "generate" {
		return generateCSV(ec, header, delim)
	}
	out := []Item{}
	for i, item := range ec.Items {
		text, err := ec.String("text", item, i)
		if err != nil {
			return nil, err
		}
		rows, err := parseCSV(text, header, delim)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		out = append(out, rows...)
	}
	return Output{"main": out}, nil
}

func delimiterRune(delim, sample string) rune {
	switch delim {
	case ",", ";", "|":
		return rune(delim[0])
	case "tab":
		return '\t'
	}
	line, _, _ := strings.Cut(sample, "\n")
	best, n := ',', -1
	for _, r := range []rune{',', ';', '\t', '|'} {
		if c := strings.Count(line, string(r)); c > n {
			best, n = r, c
		}
	}
	return best
}

func parseCSV(text string, header bool, delim string) ([]Item, error) {
	if !strings.ContainsAny(text, ",;\t|\n") {
		if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text)); err == nil && utf8.Valid(b) {
			text = string(b)
		}
	}
	text = strings.TrimPrefix(text, "\ufeff") // Excel's byte order mark
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = delimiterRune(delim, text)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	var cols []string
	out := []Item{}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if cols == nil && header {
			cols = make([]string, len(rec))
			for i, c := range rec {
				if cols[i] = strings.TrimSpace(c); cols[i] == "" {
					cols[i] = fmt.Sprintf("column%d", i+1)
				}
			}
			continue
		}
		if len(out) >= maxCSVRows {
			return nil, fmt.Errorf("more than %d rows", maxCSVRows)
		}
		it := Item{}
		for i, v := range rec {
			name := fmt.Sprintf("column%d", i+1)
			if i < len(cols) {
				name = cols[i]
			}
			it[name] = v
		}
		out = append(out, it)
	}
	return out, nil
}

func generateCSV(ec *ExecContext, header bool, delim string) (Output, error) {
	cols := ParseList(ec.Raw("columns"))
	if len(cols) == 0 { // all top-level fields, in order of first appearance
		seen := map[string]bool{}
		for _, item := range ec.Items {
			keys := make([]string, 0, len(item))
			for k := range item {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if !seen[k] {
					seen[k] = true
					cols = append(cols, k)
				}
			}
		}
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if delim != "auto" && delim != "" {
		w.Comma = delimiterRune(delim, "")
	}
	if header {
		_ = w.Write(cols)
	}
	for _, item := range ec.Items {
		rec := make([]string, len(cols))
		for i, c := range cols {
			if v, ok := getPath(item, c); ok {
				rec[i] = expression.Stringify(v)
			}
		}
		_ = w.Write(rec)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, err
	}
	name, err := ec.String("filename", first(ec.Items), 0)
	if err != nil {
		return nil, err
	}
	if name = strings.TrimSpace(name); name == "" {
		name = "export.csv"
	}
	return Output{"main": {{
		"csv": buf.String(), "content": base64.StdEncoding.EncodeToString(buf.Bytes()),
		"filename": name, "contentType": "text/csv", "rows": len(ec.Items),
	}}}, nil
}
