// Package dcron parses the five-field cron expressions accepted by the
// built-in clock's `alarms` setting.
//
// The grammar is deliberately smaller than a general cron implementation:
//
//	entry      = expression [ " @" zone ] [ " !once" ] [ " !disabled" ]
//	expression = minute SP hour SP day-of-month SP month SP day-of-week
//	field      = item { "," item }
//	item       = "*" | "*" "/" step | number | number "-" number |
//	             number "-" number "/" step
//
// Numbers are decimal with no names or 7/8-bit extensions. minute 0-59,
// hour 0-23, day-of-month 1-31, month 1-12 and day-of-week 0-7 where both 0
// and 7 mean Sunday. Fields are whitespace separated; lists are comma
// separated with no whitespace after a comma. When both day-of-month and
// day-of-week are restricted the classic cron rule applies: a minute matches
// if EITHER field matches.
//
// A stored list joins expressions with a comma that follows the completed
// fifth field. The canonical form is "expr1, expr2": because a comma inside a
// field is never followed by whitespace, a comma followed by whitespace is
// always an entry separator. ParseList consumes exactly five whitespace
// separated fields per entry, so a minute list like "0,30 7 * * 1,3,5" is one
// expression and cannot be split naively on every comma.
package dcron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is one parsed expression.
type Schedule struct {
	// Text is the normalized expression: fields separated by single spaces,
	// field contents preserved byte-for-byte.
	Text string

	minute field
	hour   field
	dom    field
	month  field
	dow    field

	domRestricted bool
	dowRestricted bool
}

type field struct {
	set    [64]bool // 64 entries covers every field's value range.
	lo, hi int
}

func (f *field) has(v int) bool {
	return v >= f.lo && v <= f.hi && f.set[v]
}

// Parse validates one five-field expression.
func Parse(expr string) (Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("cron expression %q must have exactly five fields", strings.TrimSpace(expr))
	}
	var s Schedule
	var err error
	if s.minute, _, err = parseField(fields[0], 0, 59); err != nil {
		return Schedule{}, fmt.Errorf("minute: %w", err)
	}
	if s.hour, _, err = parseField(fields[1], 0, 23); err != nil {
		return Schedule{}, fmt.Errorf("hour: %w", err)
	}
	dom, domAll, err := parseField(fields[2], 1, 31)
	if err != nil {
		return Schedule{}, fmt.Errorf("day-of-month: %w", err)
	}
	s.dom = dom
	s.domRestricted = !domAll
	if s.month, _, err = parseField(fields[3], 1, 12); err != nil {
		return Schedule{}, fmt.Errorf("month: %w", err)
	}
	dow, dowAll, err := parseField(fields[4], 0, 7)
	if err != nil {
		return Schedule{}, fmt.Errorf("day-of-week: %w", err)
	}
	if dow.set[7] {
		dow.set[0] = true
	}
	s.dow = dow
	s.dowRestricted = !dowAll
	s.Text = strings.Join(fields, " ")
	return s, nil
}

func parseField(text string, lo, hi int) (field, bool, error) {
	out := field{lo: lo, hi: hi}
	items := strings.Split(text, ",")
	if text == "" || len(items) == 0 {
		return out, false, fmt.Errorf("empty field")
	}
	for _, item := range items {
		if item == "" {
			return out, false, fmt.Errorf("empty list element in %q", text)
		}
		if strings.ContainsAny(item, " \t") {
			return out, false, fmt.Errorf("whitespace inside field %q", text)
		}
		if err := parseItem(&out, item, lo, hi); err != nil {
			return out, false, err
		}
	}
	// A field is a wildcard when it covers every legal value, not only when it
	// is a literal "*". This keeps standard cron DOM/DOW OR semantics for
	// "*/1", "*,5" and exhaustive ranges.
	all := true
	for v := lo; v <= hi; v++ {
		if !out.set[v] {
			all = false
			break
		}
	}
	return out, all, nil
}

func parseItem(out *field, item string, lo, hi int) error {
	base := item
	step := 1
	if slash := strings.IndexByte(item, '/'); slash >= 0 {
		base = item[:slash]
		// The documented grammar allows only */n and a-b/n. A bare N/step
		// (meaning N-max/step in some cron dialects) is rejected rather than
		// silently reduced to the single value N.
		if base != "*" && !strings.Contains(base, "-") {
			return fmt.Errorf("step in %q requires * or a range", item)
		}
		n, err := strconv.Atoi(item[slash+1:])
		if err != nil || n < 1 {
			return fmt.Errorf("invalid step in %q", item)
		}
		step = n
	}
	start, end := lo, hi
	switch {
	case base == "*":
	case strings.Contains(base, "-"):
		parts := strings.Split(base, "-")
		if len(parts) != 2 {
			return fmt.Errorf("invalid range %q", item)
		}
		a, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("invalid range start in %q", item)
		}
		b, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("invalid range end in %q", item)
		}
		if a < lo || b > hi || a > b {
			return fmt.Errorf("range %q is outside %d-%d", item, lo, hi)
		}
		start, end = a, b
	default:
		n, err := strconv.Atoi(base)
		if err != nil {
			return fmt.Errorf("invalid value %q", item)
		}
		if n < lo || n > hi {
			return fmt.Errorf("value %q is outside %d-%d", item, lo, hi)
		}
		start, end = n, n
	}
	for v := start; v <= end; v += step {
		out.set[v] = true
	}
	return nil
}

// Entry is one alarm schedule plus its persisted modifiers. The schedule is
// always a five-field dcron expression; Zone, Once and Disabled are optional
// suffixes in the canonical text:
//
//	expr [ " @" IANA-zone ] [ " !once" ] [ " !disabled" ]
//
// A one-shot entry fires at its next match and is then removed by the clock.
// A disabled entry stays configured (and controllable) without firing.
type Entry struct {
	Expr     string
	Zone     string
	Once     bool
	Disabled bool
}

// ID is the stable alarm identity: the expression and its immutable
// modifiers. Toggling enabled does not change it.
func (e Entry) ID() string { return e.identity(false) }

// Canonical is the persisted entry text.
func (e Entry) Canonical() string { return e.identity(true) }

func (e Entry) identity(withDisabled bool) string {
	out := e.Expr
	if e.Zone != "" {
		out += " @" + e.Zone
	}
	if e.Once {
		out += " !once"
	}
	if withDisabled && e.Disabled {
		out += " !disabled"
	}
	return out
}

// ParseList splits a stored `alarms` value into normalized expressions. It is
// the expression-only convenience form; entries with modifiers must use
// ParseEntries.
func ParseList(value string) ([]string, error) {
	entries, err := ParseEntries(value)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Expr)
	}
	return out, nil
}

// ParseEntries splits a stored `alarms` value into canonical entries.
func ParseEntries(value string) ([]Entry, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	tokens := strings.Fields(value)
	var out []Entry
	for i := 0; i < len(tokens); {
		fields := make([]string, 0, 5)
		separated := false
		for len(fields) < 5 {
			if i >= len(tokens) {
				return nil, fmt.Errorf("alarm %d has %d fields; five are required", len(out)+1, len(fields))
			}
			tok := tokens[i]
			i++
			if tok == "," {
				return nil, fmt.Errorf("alarm %d has fewer than five fields", len(out)+1)
			}
			if strings.HasSuffix(tok, ",") {
				if len(fields) != 4 {
					return nil, fmt.Errorf("unexpected comma in field %d of alarm %d", len(fields)+1, len(out)+1)
				}
				tok = strings.TrimSuffix(tok, ",")
				if tok == "" {
					return nil, fmt.Errorf("unexpected comma in alarm %d", len(out)+1)
				}
				separated = true
			}
			fields = append(fields, tok)
		}
		entry := Entry{Expr: strings.Join(fields, " ")}
		if _, err := Parse(entry.Expr); err != nil {
			return nil, fmt.Errorf("alarm %d: %w", len(out)+1, err)
		}
		if separated {
			// A separator immediately after the fifth field means no
			// modifiers may follow.
			out = append(out, entry)
			if i >= len(tokens) {
				return nil, fmt.Errorf("alarm list ends with a separator")
			}
			continue
		}
		for i < len(tokens) {
			tok := tokens[i]
			if tok == "," {
				break
			}
			if !strings.HasPrefix(tok, "@") && !strings.HasPrefix(tok, "!") {
				break
			}
			i++
			if strings.HasSuffix(tok, ",") {
				tok = strings.TrimSuffix(tok, ",")
				separated = true
			}
			switch {
			case strings.HasPrefix(tok, "@"):
				zone := strings.TrimPrefix(tok, "@")
				if zone == "" || entry.Zone != "" {
					return nil, fmt.Errorf("alarm %d has an invalid or duplicate timezone", len(out)+1)
				}
				entry.Zone = zone
			case tok == "!once":
				if entry.Once {
					return nil, fmt.Errorf("alarm %d repeats !once", len(out)+1)
				}
				entry.Once = true
			case tok == "!disabled":
				if entry.Disabled {
					return nil, fmt.Errorf("alarm %d repeats !disabled", len(out)+1)
				}
				entry.Disabled = true
			default:
				return nil, fmt.Errorf("alarm %d has unknown modifier %q", len(out)+1, tok)
			}
			if separated {
				break
			}
		}
		out = append(out, entry)
		if separated {
			if i >= len(tokens) {
				return nil, fmt.Errorf("alarm list ends with a separator")
			}
			continue
		}
		if i >= len(tokens) {
			break
		}
		if tokens[i] == "," {
			i++
			if i >= len(tokens) {
				return nil, fmt.Errorf("alarm list ends with a separator")
			}
			continue
		}
		return nil, fmt.Errorf("alarms must be separated by a comma after the fifth field")
	}
	return out, nil
}

// ParseEntry parses exactly one canonical entry (no separator).
func ParseEntry(value string) (Entry, error) {
	entries, err := ParseEntries(value)
	if err != nil {
		return Entry{}, err
	}
	if len(entries) != 1 {
		return Entry{}, fmt.Errorf("expected one alarm entry")
	}
	return entries[0], nil
}

// FormatEntries joins entries with the canonical comma-and-space separator.
func FormatEntries(entries []Entry) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.Canonical())
	}
	return strings.Join(parts, ", ")
}

// FormatList joins expression-only entries with the canonical separator.
func FormatList(exprs []string) string {
	return strings.Join(exprs, ", ")
}

// Matches reports whether a local wall-clock minute matches the schedule.
func (s Schedule) Matches(now time.Time, loc *time.Location) bool {
	t := now.In(loc)
	if !s.month.has(int(t.Month())) || !s.hour.has(t.Hour()) || !s.minute.has(t.Minute()) {
		return false
	}
	dom := s.dom.has(t.Day())
	dow := s.dow.has(int(t.Weekday()))
	switch {
	case s.domRestricted && s.dowRestricted:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestricted:
		return dow
	default:
		return true
	}
}

// Next returns the first matching minute strictly after now. A legitimate
// schedule always matches within four years (February 29 is the longest gap).
func (s Schedule) Next(now time.Time, loc *time.Location) time.Time {
	t := now.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(4, 0, 0)
	for !t.After(limit) {
		if !s.month.has(int(t.Month())) {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		for minute := range 24 * 60 {
			cand := dayStart.Add(time.Duration(minute) * time.Minute)
			if cand.Before(t) {
				continue
			}
			local := cand.In(loc)
			if local.Year() != t.Year() || local.Month() != t.Month() || local.Day() != t.Day() {
				// A DST transition can skip or repeat wall time; resuming from
				// the next real instant keeps the scan monotonic.
				break
			}
			if s.month.has(int(local.Month())) && s.hour.has(local.Hour()) && s.minute.has(local.Minute()) {
				return cand
			}
		}
		t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
	}
	return time.Time{}
}

func (s Schedule) dayMatches(t time.Time) bool {
	dom := s.dom.has(t.Day())
	dow := s.dow.has(int(t.Weekday()))
	switch {
	case s.domRestricted && s.dowRestricted:
		return dom || dow
	case s.domRestricted:
		return dom
	case s.dowRestricted:
		return dow
	default:
		return true
	}
}
