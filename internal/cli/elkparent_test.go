package cli

import (
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

func TestParseElkParentReadsOnlyTheFirstLine(t *testing.T) {
	cases := map[string]struct {
		ref string
		ok  bool
	}{
		"elk-parent: #35":                       {"#35", true},
		"elk-parent: 35\nignored: everything":   {"35", true},
		"ELK-PARENT:elk:#35":                    {"elk:#35", true},
		"  elk-parent:   80cc9ec0-842b   \r\nx": {"80cc9ec0-842b", true},
		"elk-parent:":                           {"", false},
		"Started on this":                       {"", false},
		"note\nelk-parent: #35":                 {"", false},
		"":                                      {"", false},
	}
	for body, want := range cases {
		ref, ok := parseElkParent(body)
		if ok != want.ok || ref != want.ref {
			t.Errorf("parseElkParent(%q) = (%q, %v), want (%q, %v)", body, ref, ok, want.ref, want.ok)
		}
	}
}

func TestLatestElkParentWins(t *testing.T) {
	comments := []*store.Comment{
		{Body: "elk-parent: #35"},
		{Body: "just a comment"},
		{Body: "elk-parent: #36"},
	}
	if got := latestElkParent(comments); got != "#36" {
		t.Errorf("latestElkParent = %q, want #36", got)
	}
	if got := latestElkParent(nil); got != "" {
		t.Errorf("latestElkParent(nil) = %q, want empty", got)
	}
}

func TestElkRefKeyTreatsTheGrammarsAsOne(t *testing.T) {
	for _, r := range []string{"#35", "35", "elk:#35", "elk:35", "ELK:35", " #35 "} {
		if got := elkRefKey(r); got != "35" {
			t.Errorf("elkRefKey(%q) = %q, want 35", r, got)
		}
	}
	if got := elkRefKey("80CC9EC0-842B"); got != "80cc9ec0-842b" {
		t.Errorf("elkRefKey(uuid) = %q", got)
	}
}

func TestValidElkRefIsOneNonEmptyLine(t *testing.T) {
	if got, err := validElkRef("  #35 "); err != nil || got != "#35" {
		t.Fatalf("validElkRef = (%q, %v)", got, err)
	}
	for _, bad := range []string{"", "   ", "#35\n#36"} {
		_, err := validElkRef(bad)
		if err == nil || records.ExitCode(err) != 2 {
			t.Errorf("validElkRef(%q) err = %v, want exit 2", bad, err)
		}
	}
}

func TestElkParentMarkerShape(t *testing.T) {
	if got := elkParentMarker("#35"); got != "elk-parent: #35" {
		t.Errorf("elkParentMarker = %q", got)
	}
}
