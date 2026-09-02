package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

func TestConfigSetAndGetRequireElkParent(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	var got struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	arkJSON(t, dir, &got, "config", "get", "require-elk-parent")
	if got.Value != "false" {
		t.Fatalf("default require-elk-parent = %q, want false", got.Value)
	}

	out := ark(t, dir, "config", "set", "require-elk-parent", "true")
	if !strings.Contains(out, "require-elk-parent = true") {
		t.Fatalf("set output: %s", out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".ark", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "require_elk_parent = true") {
		t.Fatalf("config.toml after set:\n%s", raw)
	}
	arkJSON(t, dir, &got, "config", "get", "require-elk-parent")
	if got.Value != "true" {
		t.Fatalf("require-elk-parent after set = %q", got.Value)
	}

	ark(t, dir, "config", "set", "require-elk-parent", "false")
	arkJSON(t, dir, &got, "config", "get", "require-elk-parent")
	if got.Value != "false" {
		t.Fatalf("require-elk-parent after unset = %q", got.Value)
	}
}

func TestConfigRejectsUnknownKeysAndBadValues(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	if _, err := arkErr(t, dir, "config", "get", "colour"); err == nil || records.ExitCode(err) != 2 {
		t.Errorf("unknown key: err = %v, want exit 2", err)
	}
	if _, err := arkErr(t, dir, "config", "set", "require-elk-parent", "maybe"); err == nil || records.ExitCode(err) != 2 {
		t.Errorf("bad bool: err = %v, want exit 2", err)
	}
}
