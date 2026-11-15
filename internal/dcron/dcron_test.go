package dcron

import (
	"testing"
	"time"
)

func TestParseListKeepsIntraFieldCommas(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"0,30 7 * * 1,3,5", []string{"0,30 7 * * 1,3,5"}},
		{"0,30 7 * * 1,3,5, 15 8 * * *", []string{"0,30 7 * * 1,3,5", "15 8 * * *"}},
		{"  0 7 * * *  ,  30 8 * * * ", []string{"0 7 * * *", "30 8 * * *"}},
		{"*/15 9-17 * * 1-5", []string{"*/15 9-17 * * 1-5"}},
	}
	for _, tc := range cases {
		got, err := ParseList(tc.in)
		if err != nil {
			t.Fatalf("ParseList(%q): %v", tc.in, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("ParseList(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("ParseList(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseListRejectsAmbiguousOrEmptyEntries(t *testing.T) {
	for _, in := range []string{
		"0,30, 7 * * *",          // trailing comma inside a non-final field
		"0 7 * * *, , 0 8 * * *", // empty entry
		"0 7 * * *,",             // trailing separator
		"0 7 * *",                // four fields
		"0 7 * * * 0 8 * * *",    // missing separator
		"0/15 9 * * *",           // bare value/step is not in the grammar
	} {
		if _, err := ParseList(in); err == nil {
			t.Fatalf("ParseList(%q) accepted an invalid list", in)
		}
	}
}

func TestParseEntriesPreservesModifiers(t *testing.T) {
	text := "30 6 * * *, 0,30 7 * * 1,3,5 @Europe/Berlin !once, 15 8 * * * !disabled"
	entries, err := ParseEntries(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Canonical() != "30 6 * * *" || entries[0].ID() != "30 6 * * *" {
		t.Fatalf("first = %q / %q", entries[0].Canonical(), entries[0].ID())
	}
	if entries[1].Zone != "Europe/Berlin" || !entries[1].Once || entries[1].Canonical() != "0,30 7 * * 1,3,5 @Europe/Berlin !once" {
		t.Fatalf("second = %+v", entries[1])
	}
	if entries[1].ID() != "0,30 7 * * 1,3,5 @Europe/Berlin !once" {
		t.Fatalf("identity = %q", entries[1].ID())
	}
	if !entries[2].Disabled || entries[2].Canonical() != "15 8 * * * !disabled" {
		t.Fatalf("third = %+v", entries[2])
	}
	if got := FormatEntries(entries); got != text {
		t.Fatalf("round trip = %q, want %q", got, text)
	}
}

func TestParseEntriesRejectsBadModifiers(t *testing.T) {
	for _, in := range []string{
		"0 7 * * * @",
		"0 7 * * * !unknown",
		"0 7 * * * @UTC @UTC",
		"0 7 * * * !once !once",
		"0 7 * * *, @UTC",
	} {
		if _, err := ParseEntries(in); err == nil {
			t.Fatalf("ParseEntries(%q) accepted invalid modifiers", in)
		}
	}
}

func TestFormatListRoundTrip(t *testing.T) {
	exprs := []string{"0,30 7 * * 1,3,5", "15 8 * * *"}
	text := FormatList(exprs)
	if text != "0,30 7 * * 1,3,5, 15 8 * * *" {
		t.Fatalf("FormatList = %q", text)
	}
	back, err := ParseList(text)
	if err != nil {
		t.Fatal(err)
	}
	for i := range exprs {
		if back[i] != exprs[i] {
			t.Fatalf("round trip [%d] = %q, want %q", i, back[i], exprs[i])
		}
	}
}

func TestNextAndMatches(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, 11, 11, 9, 0, 30, 0, utc) // Wednesday
	s, err := Parse("0,30 9 * * 1,3,5")
	if err != nil {
		t.Fatal(err)
	}
	next := s.Next(now, utc)
	if want := time.Date(2026, 11, 11, 9, 30, 0, 0, utc); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
	if !s.Matches(time.Date(2026, 11, 13, 9, 30, 0, 0, utc), utc) {
		t.Fatal("Friday did not match a Mon/Wed/Fri schedule")
	}
	if s.Matches(time.Date(2026, 11, 14, 9, 30, 0, 0, utc), utc) {
		t.Fatal("Saturday matched a Mon/Wed/Fri schedule")
	}

	// 0 and 7 both mean Sunday.
	sunday, err := Parse("0 8 * * 7")
	if err != nil {
		t.Fatal(err)
	}
	if !sunday.Matches(time.Date(2026, 11, 15, 8, 0, 0, 0, utc), utc) {
		t.Fatal("day-of-week 7 did not match Sunday")
	}

	// Restricted day-of-month OR day-of-week (standard cron rule).
	or, err := Parse("0 8 1 * 5")
	if err != nil {
		t.Fatal(err)
	}
	if !or.Matches(time.Date(2026, 12, 1, 8, 0, 0, 0, utc), utc) {
		t.Fatal("first of the month did not match")
	}
	if !or.Matches(time.Date(2026, 11, 13, 8, 0, 0, 0, utc), utc) {
		t.Fatal("Friday did not match")
	}
	if or.Matches(time.Date(2026, 11, 14, 8, 0, 0, 0, utc), utc) {
		t.Fatal("Saturday the 14th matched neither field but was accepted")
	}
}

func TestWildcardDerivedFieldKeepsDOWSemantics(t *testing.T) {
	utc := time.UTC
	s, err := Parse("0 7 */1 * 1")
	if err != nil {
		t.Fatal(err)
	}
	monday := time.Date(2026, 11, 16, 7, 0, 0, 0, utc)
	tuesday := time.Date(2026, 11, 17, 7, 0, 0, 0, utc)
	if !s.Matches(monday, utc) {
		t.Fatal("Monday did not match 0 7 */1 * 1")
	}
	if s.Matches(tuesday, utc) {
		t.Fatal("*/1 day-of-month must stay a wildcard; Tuesday fired")
	}
	// An exhaustive range or mixed wildcard list is also unrestricted.
	for _, expr := range []string{"0 7 1-31 * 1", "0 7 *,5 * 1"} {
		parsed, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Matches(tuesday, utc) {
			t.Fatalf("%s treated day-of-month as restricted", expr)
		}
	}
}

func TestDSTGapDoesNotReturnNonexistentTime(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}
	s, err := Parse("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 8, 1, 0, 0, 0, ny)
	next := s.Next(now, ny)
	if next.IsZero() || !next.After(now) {
		t.Fatalf("next = %v", next)
	}
	if local := next.In(ny); local.Hour() != 2 || local.Minute() != 30 {
		t.Fatalf("next local = %v, want a real 02:30", local)
	}
}
