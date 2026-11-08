package nodes

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/glue2flow/g2f/internal/expression"
)

type dateTimeNode struct{}

var dateUnits = []Option{
	{"Seconds", "seconds"}, {"Minutes", "minutes"}, {"Hours", "hours"}, {"Days", "days"},
	{"Weeks", "weeks"}, {"Months", "months"}, {"Years", "years"},
}

var dateFormats = []Option{
	{"ISO 8601 (2026-03-01T14:05:00+01:00)", "iso"},
	{"Date (2026-03-01)", "date"},
	{"Date and time (2026-03-01 14:05)", "datetime"},
	{"Time (14:05)", "time"},
	{"E-mail / RFC 2822 (Sun, 01 Mar 2026 14:05:00 +0100)", "rfc2822"},
	{"Unix seconds", "unix"},
	{"Unix milliseconds", "unixMs"},
	{"Custom", "custom"},
}

func (dateTimeNode) Definition() Definition {
	show := func(ops ...string) map[string][]string { return map[string][]string{"operation": ops} }
	return dataDef("data.dateTime", "Date & Time",
		"Format, parse and calculate dates in any timezone: add or subtract time, start of day/week/month, "+
			"difference between two dates.", []string{"main"},
		[]Param{
			{Name: "operation", Label: "Operation", Type: ParamSelect, Default: "format", NoExpression: true, Options: []Option{
				{"Format a date", "format"}, {"Add time", "add"}, {"Subtract time", "subtract"},
				{"Start of a period", "startOf"}, {"End of a period", "endOf"},
				{"Difference between two dates", "difference"}, {"Parse text with a custom format", "parse"},
			}},
			{Name: "date", Label: "Date", Required: true, Default: "{{ now() }}",
				Description: "ISO text, a date, e-mail date, Unix seconds or milliseconds, or now()."},
			{Name: "endDate", Label: "End date", Required: true, Default: "{{ now() }}", ShowIf: show("difference")},
			{Name: "amount", Label: "Amount", Type: ParamNumber, Default: 1, ShowIf: show("add", "subtract")},
			{Name: "unit", Label: "Unit", Type: ParamSelect, Default: "days", NoExpression: true, Options: dateUnits,
				ShowIf: show("add", "subtract", "difference")},
			{Name: "period", Label: "Period", Type: ParamSelect, Default: "day", NoExpression: true, ShowIf: show("startOf", "endOf"),
				Options: []Option{{"Hour", "hour"}, {"Day", "day"}, {"Week (Monday)", "week"}, {"Month", "month"}, {"Quarter", "quarter"}, {"Year", "year"}}},
			{Name: "inputFormat", Label: "Input format", Required: true, Placeholder: "dd.MM.yyyy HH:mm", ShowIf: show("parse"),
				Description: "Tokens: yyyy yy MM M MMM MMMM dd d HH H hh h mm ss SSS a EEE EEEE Z; text in 'quotes'."},
			{Name: "format", Label: "Output format", Type: ParamSelect, Default: "iso", NoExpression: true, Options: dateFormats,
				ShowIf: show("format", "add", "subtract", "startOf", "endOf", "parse")},
			{Name: "customFormat", Label: "Custom format", Default: "dd.MM.yyyy HH:mm", ShowIf: map[string][]string{"format": {"custom"}},
				Description: "Tokens: yyyy yy MM M MMM MMMM dd d EEE EEEE HH H hh h mm m ss s SSS a Z ZZ z; text in 'quotes'."},
			{Name: "timezone", Label: "Timezone", Default: "UTC", Placeholder: "Europe/Berlin",
				Description: "For output, periods and dates without a zone."},
			{Name: "field", Label: "Result field", Default: "date", NoExpression: true},
		},
		&OutputSchema{Passthrough: true, Fields: fields("date:any:Result (default result field)")})
}

func (dateTimeNode) Execute(_ context.Context, ec *ExecContext) (Output, error) {
	op, _ := ec.Raw("operation").(string)
	if op == "" {
		op = "format"
	}
	field, _ := ec.Raw("field").(string)
	if field = strings.TrimSpace(field); field == "" {
		field = "date"
	}
	out := make([]Item, 0, len(ec.Items))
	for i, item := range ec.Items {
		res, err := dateOp(ec, op, item, i)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		next := make(Item, len(item)+1)
		maps.Copy(next, item)
		next[field] = res
		out = append(out, next)
	}
	return Output{"main": out}, nil
}

func dateOp(ec *ExecContext, op string, item Item, i int) (any, error) {
	tzName, err := ec.String("timezone", item, i)
	if err != nil {
		return nil, err
	}
	loc := time.UTC
	if tzName = strings.TrimSpace(tzName); tzName != "" {
		if loc, err = time.LoadLocation(tzName); err != nil {
			return nil, fmt.Errorf("unknown timezone %q", tzName)
		}
	}
	date := func(name string) (time.Time, error) {
		v, err := ec.Param(name, item, i)
		if err != nil {
			return time.Time{}, err
		}
		return ParseDate(v, loc)
	}

	var t time.Time
	switch op {
	case "parse":
		v, err := ec.String("date", item, i)
		if err != nil {
			return nil, err
		}
		layout, err := ec.String("inputFormat", item, i)
		if err != nil {
			return nil, err
		}
		if t, err = time.ParseInLocation(goLayout(layout), strings.TrimSpace(v), loc); err != nil {
			return nil, fmt.Errorf("%q does not match the format %q", v, layout)
		}
	case "difference":
		a, err := date("date")
		if err != nil {
			return nil, err
		}
		b, err := date("endDate")
		if err != nil {
			return nil, err
		}
		unit, _ := ec.Raw("unit").(string)
		return dateDiff(a, b, unit)
	default:
		if t, err = date("date"); err != nil {
			return nil, err
		}
	}
	t = t.In(loc)

	switch op {
	case "add", "subtract":
		amount, err := ec.Number("amount", item, i)
		if err != nil {
			return nil, err
		}
		if op == "subtract" {
			amount = -amount
		}
		unit, _ := ec.Raw("unit").(string)
		if t, err = addDate(t, amount, unit); err != nil {
			return nil, err
		}
	case "startOf", "endOf":
		period, _ := ec.Raw("period").(string)
		start, next, err := periodBounds(t, period)
		if err != nil {
			return nil, err
		}
		t = start
		if op == "endOf" {
			t = next.Add(-time.Millisecond)
		}
	case "format", "parse":
	default:
		return nil, fmt.Errorf("unknown operation %q", op)
	}

	format, _ := ec.Raw("format").(string)
	custom, err := ec.String("customFormat", item, i)
	if err != nil {
		return nil, err
	}
	return FormatDate(t, format, custom), nil
}

// ParseDate reads a date from workflow data: time.Time, Unix seconds or
// milliseconds, or text (ISO 8601 / RFC 3339, date only, "yyyy-MM-dd HH:mm[:ss]",
// e-mail dates). Text without a zone is read in loc.
func ParseDate(v any, loc *time.Location) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case nil:
		return time.Time{}, fmt.Errorf("date is empty")
	case float64, int, int64:
		f, _ := toFloat(t)
		return unixTime(f), nil
	}
	s := strings.TrimSpace(expression.Stringify(v))
	if s == "" {
		return time.Time{}, fmt.Errorf("date is empty")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !strings.Contains(s, "-") {
		return unixTime(f), nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC1123, "Mon, 2 Jan 2006 15:04:05 -0700", "2 Jan 2006 15:04:05 -0700", time.RFC850} {
		if tm, err := time.Parse(layout, s); err == nil {
			return tm, nil
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if tm, err := time.ParseInLocation(layout, s, loc); err == nil {
			return tm, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a date", s)
}

// unixTime treats values above 1e11 as milliseconds (dates after 1973).
func unixTime(f float64) time.Time {
	if math.Abs(f) > 1e11 {
		return time.UnixMilli(int64(f)).UTC()
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

func addDate(t time.Time, amount float64, unit string) (time.Time, error) {
	whole := amount == math.Trunc(amount)
	switch unit {
	case "seconds":
		return t.Add(time.Duration(amount * float64(time.Second))), nil
	case "minutes":
		return t.Add(time.Duration(amount * float64(time.Minute))), nil
	case "hours":
		return t.Add(time.Duration(amount * float64(time.Hour))), nil
	case "days", "":
		if whole {
			return t.AddDate(0, 0, int(amount)), nil // calendar days: keeps the wall clock across DST
		}
		return t.Add(time.Duration(amount * float64(day))), nil
	case "weeks":
		return addDate(t, amount*7, "days")
	case "months", "years":
		if !whole {
			return t, fmt.Errorf("%s must be whole numbers", unit)
		}
		if unit == "years" {
			return t.AddDate(int(amount), 0, 0), nil
		}
		return t.AddDate(0, int(amount), 0), nil
	}
	return t, fmt.Errorf("unknown unit %q", unit)
}

// dateDiff returns b - a in unit (months and years as whole calendar units).
func dateDiff(a, b time.Time, unit string) (float64, error) {
	d := b.Sub(a)
	switch unit {
	case "seconds":
		return d.Seconds(), nil
	case "minutes":
		return d.Minutes(), nil
	case "hours":
		return d.Hours(), nil
	case "days", "":
		return d.Hours() / 24, nil
	case "weeks":
		return d.Hours() / (24 * 7), nil
	case "months", "years":
		sign := 1.0
		if b.Before(a) {
			a, b, sign = b, a, -1
		}
		b = b.In(a.Location())
		months := (b.Year()-a.Year())*12 + int(b.Month()-a.Month())
		if a.AddDate(0, months, 0).After(b) {
			months--
		}
		if unit == "years" {
			return sign * float64(months/12), nil
		}
		return sign * float64(months), nil
	}
	return 0, fmt.Errorf("unknown unit %q", unit)
}

// periodBounds returns the start of the period containing t and the start of
// the next one, in t's location.
func periodBounds(t time.Time, period string) (time.Time, time.Time, error) {
	y, m, d := t.Date()
	loc := t.Location()
	switch period {
	case "hour":
		s := time.Date(y, m, d, t.Hour(), 0, 0, 0, loc)
		return s, s.Add(time.Hour), nil
	case "day", "":
		s := time.Date(y, m, d, 0, 0, 0, 0, loc)
		return s, s.AddDate(0, 0, 1), nil
	case "week":
		offset := (int(t.Weekday()) + 6) % 7 // Monday = 0
		s := time.Date(y, m, d-offset, 0, 0, 0, 0, loc)
		return s, s.AddDate(0, 0, 7), nil
	case "month":
		s := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		return s, s.AddDate(0, 1, 0), nil
	case "quarter":
		s := time.Date(y, m-(m-1)%3, 1, 0, 0, 0, 0, loc)
		return s, s.AddDate(0, 3, 0), nil
	case "year":
		s := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
		return s, s.AddDate(1, 0, 0), nil
	}
	return t, t, fmt.Errorf("unknown period %q", period)
}

// FormatDate renders t in one of the preset formats or a custom token format.
func FormatDate(t time.Time, format, custom string) any {
	switch format {
	case "", "iso":
		if t.Nanosecond()%int(time.Millisecond) == 0 && t.Nanosecond() != 0 {
			return t.Format("2006-01-02T15:04:05.000Z07:00")
		}
		return t.Format(time.RFC3339)
	case "date":
		return t.Format("2006-01-02")
	case "datetime":
		return t.Format("2006-01-02 15:04")
	case "time":
		return t.Format("15:04")
	case "rfc2822":
		return t.Format(time.RFC1123Z)
	case "unix":
		return t.Unix()
	case "unixMs":
		return t.UnixMilli()
	}
	return formatTokens(t, custom)
}

// dateTokens are matched longest first.
var dateTokens = []string{"yyyy", "yy", "MMMM", "MMM", "MM", "M", "dd", "d", "EEEE", "EEE", "HH", "H", "hh", "h", "mm", "m", "ss", "s", "SSS", "a", "ZZ", "Z", "z"}

func formatTokens(t time.Time, f string) string {
	var b strings.Builder
	for i := 0; i < len(f); {
		if f[i] == '\'' { // quoted literal; '' is a quote
			j := strings.IndexByte(f[i+1:], '\'')
			if j < 0 {
				b.WriteString(f[i+1:])
				break
			}
			if j == 0 {
				b.WriteByte('\'')
			} else {
				b.WriteString(f[i+1 : i+1+j])
			}
			i += j + 2
			continue
		}
		tok := ""
		for _, k := range dateTokens {
			if strings.HasPrefix(f[i:], k) {
				tok = k
				break
			}
		}
		if tok == "" {
			b.WriteByte(f[i])
			i++
			continue
		}
		h12 := t.Hour() % 12
		if h12 == 0 {
			h12 = 12
		}
		switch tok {
		case "yyyy":
			fmt.Fprintf(&b, "%04d", t.Year())
		case "yy":
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case "MMMM":
			b.WriteString(t.Month().String())
		case "MMM":
			b.WriteString(t.Month().String()[:3])
		case "MM":
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case "M":
			fmt.Fprintf(&b, "%d", int(t.Month()))
		case "dd":
			fmt.Fprintf(&b, "%02d", t.Day())
		case "d":
			fmt.Fprintf(&b, "%d", t.Day())
		case "EEEE":
			b.WriteString(t.Weekday().String())
		case "EEE":
			b.WriteString(t.Weekday().String()[:3])
		case "HH":
			fmt.Fprintf(&b, "%02d", t.Hour())
		case "H":
			fmt.Fprintf(&b, "%d", t.Hour())
		case "hh":
			fmt.Fprintf(&b, "%02d", h12)
		case "h":
			fmt.Fprintf(&b, "%d", h12)
		case "mm":
			fmt.Fprintf(&b, "%02d", t.Minute())
		case "m":
			fmt.Fprintf(&b, "%d", t.Minute())
		case "ss":
			fmt.Fprintf(&b, "%02d", t.Second())
		case "s":
			fmt.Fprintf(&b, "%d", t.Second())
		case "SSS":
			fmt.Fprintf(&b, "%03d", t.Nanosecond()/int(time.Millisecond))
		case "a":
			b.WriteString(t.Format("PM"))
		case "Z":
			b.WriteString(t.Format("-07:00"))
		case "ZZ":
			b.WriteString(t.Format("-0700"))
		case "z":
			b.WriteString(t.Format("MST"))
		}
		i += len(tok)
	}
	return b.String()
}

// goLayout translates a token format into a Go layout for parsing.
func goLayout(f string) string {
	repl := map[string]string{
		"yyyy": "2006", "yy": "06", "MMMM": "January", "MMM": "Jan", "MM": "01", "M": "1", "dd": "02", "d": "2",
		"EEEE": "Monday", "EEE": "Mon", "HH": "15", "H": "15", "hh": "03", "h": "3", "mm": "04", "m": "4",
		"ss": "05", "s": "5", "SSS": "000", "a": "PM", "ZZ": "-0700", "Z": "-07:00", "z": "MST",
	}
	var b strings.Builder
	for i := 0; i < len(f); {
		if f[i] == '\'' {
			j := strings.IndexByte(f[i+1:], '\'')
			if j < 0 {
				b.WriteString(f[i+1:])
				break
			}
			b.WriteString(f[i+1 : i+1+j])
			i += j + 2
			continue
		}
		matched := false
		for _, k := range dateTokens {
			if strings.HasPrefix(f[i:], k) {
				b.WriteString(repl[k])
				i += len(k)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(f[i])
			i++
		}
	}
	return b.String()
}
