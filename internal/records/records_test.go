package records

import (
	"errors"
	"strings"
	"testing"
)

func TestNewIDSortable(t *testing.T) {
	a := NewID()
	b := NewID()
	if len(a) != 26 || len(b) != 26 {
		t.Fatalf("ULIDs should be 26 chars: %q %q", a, b)
	}
	if !ValidID(a) {
		t.Fatalf("NewID produced invalid ULID %q", a)
	}
	if strings.Compare(a, b) > 0 {
		t.Fatalf("later ULID should not sort before earlier: %q > %q", a, b)
	}
}

func TestOneOf(t *testing.T) {
	if err := OneOf("status", "open", TaskStatuses); err != nil {
		t.Fatalf("open should be valid: %v", err)
	}
	err := OneOf("status", "bogus", TaskStatuses)
	if err == nil {
		t.Fatal("bogus should be invalid")
	}
	var ae *Error
	if !errors.As(err, &ae) || ae.Kind != KindValidation {
		t.Fatalf("want validation error, got %v", err)
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[Kind]int{
		KindGeneral:    1,
		KindValidation: 2,
		KindNotFound:   3,
		KindConflict:   4,
		KindPermission: 5,
		KindOffline:    6,
		KindPartial:    7,
		KindGit:        1,
		KindDatabase:   1,
		// 8 is the service's stored copy of this repository being unusable: a
		// 5xx that is permanent, kept off 6 because 6 is the code a retry loop
		// keys on (elk-work/ark#65).
		KindRemoteCorrupt: 8,
	}
	for kind, want := range cases {
		if got := kind.ExitCode(); got != want {
			t.Errorf("kind %d: exit %d, want %d", kind, got, want)
		}
	}
	if got := ExitCode(errors.New("plain")); got != 1 {
		t.Errorf("plain error: exit %d, want 1", got)
	}
	if got := ExitCode(NotFoundf("x")); got != 3 {
		t.Errorf("not found: exit %d, want 3", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := Truncate("multi\nline", 20); got != "multi line" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("x", 30)
	if got := Truncate(long, 10); len([]rune(got)) != 10 {
		t.Errorf("truncated length %d, want 10", len([]rune(got)))
	}
}

func TestTimeCompareOrdersTrimmedFractions(t *testing.T) {
	// Every one of these pairs is (earlier, later) chronologically, and every
	// one of them compares the wrong way round as raw strings, because
	// time.RFC3339Nano trims trailing zeros from the fractional second.
	pairs := [][2]string{
		{"2026-08-26T07:00:00.5Z", "2026-08-26T07:00:00.500000001Z"},
		{"2026-08-26T08:31:24.1724Z", "2026-08-26T08:31:24.172492Z"},
		{"2026-08-26T07:00:00Z", "2026-08-26T07:00:00.000000001Z"},
	}
	for _, p := range pairs {
		earlier, later := p[0], p[1]
		if later >= earlier {
			t.Errorf("precondition: %q should sort before %q as raw strings", later, earlier)
		}
		if !TimeBefore(earlier, later) {
			t.Errorf("TimeBefore(%q, %q) = false, want true", earlier, later)
		}
		if !TimeAfter(later, earlier) {
			t.Errorf("TimeAfter(%q, %q) = false, want true", later, earlier)
		}
		if got := TimeCompare(earlier, later); got != -1 {
			t.Errorf("TimeCompare(%q, %q) = %d, want -1", earlier, later, got)
		}
	}
}

func TestTimeCompareEquivalentSpellings(t *testing.T) {
	// The same instant written two ways is equal, not merely close.
	if got := TimeCompare("2026-08-26T07:00:00.5Z", "2026-08-26T07:00:00.500Z"); got != 0 {
		t.Errorf("TimeCompare of equal instants = %d, want 0", got)
	}
	if got := TimeCompare("2026-08-26T07:00:00Z", "2026-08-26T09:00:00+02:00"); got != 0 {
		t.Errorf("TimeCompare across offsets = %d, want 0", got)
	}
}

func TestTimeCompareFallsBackToBytes(t *testing.T) {
	// A value that is not an RFC3339 timestamp — an empty column, or a
	// caller-supplied `--since` written as a bare date — keeps the byte
	// ordering it already relied on rather than silently sorting first.
	if got := TimeCompare("", "2026-08-26T07:00:00Z"); got != -1 {
		t.Errorf("empty vs timestamp = %d, want -1", got)
	}
	if got := TimeCompare("2026-08-26T07:00:00.5Z", "2026-08-20"); got != 1 {
		t.Errorf("timestamp vs bare date = %d, want 1", got)
	}
	if got := TimeCompare("2026-08-26T07:00:00.5Z", "2026-08-27"); got != -1 {
		t.Errorf("timestamp vs later bare date = %d, want -1", got)
	}
	if got := TimeCompare("", ""); got != 0 {
		t.Errorf("empty vs empty = %d, want 0", got)
	}
}

func TestNowIsComparableWithItself(t *testing.T) {
	// records.Now() takes whatever resolution the host clock offers. On a
	// microsecond-resolution clock every value it returns is trimmed, so this
	// is the ordering that matters in practice.
	prev := Now()
	for i := 0; i < 20000; i++ {
		cur := Now()
		if TimeBefore(cur, prev) {
			t.Fatalf("Now() went backwards: %q then %q", prev, cur)
		}
		prev = cur
	}
}
