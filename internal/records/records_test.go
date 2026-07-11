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
