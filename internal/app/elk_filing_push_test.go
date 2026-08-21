package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestElkFilingPushRequiresExistingCompleteConfiguration(t *testing.T) {
	t.Setenv("ARK_ELK_ENDPOINT", "")
	t.Setenv("ARK_ELK_TOKEN", "")
	if got := newElkFilingPush(t.TempDir(), t.TempDir(), nil); got != nil {
		t.Fatal("push enabled without endpoint or token")
	}

	t.Setenv("ARK_ELK_ENDPOINT", "https://example.invalid/ark/events")
	if got := newElkFilingPush(t.TempDir(), t.TempDir(), nil); got != nil {
		t.Fatal("push enabled without token")
	}

	t.Setenv("ARK_ELK_TOKEN", "test-secret")
	if got := newElkFilingPush(t.TempDir(), t.TempDir(), nil); got == nil {
		t.Fatal("push disabled with endpoint and token configured")
	}
}

func TestElkFilingPushStartsExistingCommandWithoutSecretArguments(t *testing.T) {
	arkDir := t.TempDir()
	when := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	var gotArgs []string
	p := &elkFilingPush{
		root:       "/repo",
		arkDir:     arkDir,
		executable: "/usr/local/bin/ark",
		now:        func() time.Time { return when },
		start: func(cmd *exec.Cmd) error {
			gotArgs = append([]string(nil), cmd.Args...)
			if cmd.Stdout == nil || cmd.Stderr == nil {
				t.Fatal("detached command must retain observable output")
			}
			return nil
		},
	}

	p.launch()
	want := []string{"/usr/local/bin/ark", "--json", "-C", "/repo", "elk", "push"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args = %q, want %q", gotArgs, want)
	}
	for _, arg := range gotArgs {
		if strings.Contains(arg, "secret") || strings.Contains(arg, "example.invalid") {
			t.Fatalf("configuration leaked into process arguments: %q", gotArgs)
		}
	}
	data, err := os.ReadFile(filepath.Join(arkDir, elkPushLogName))
	if err != nil {
		t.Fatalf("read push log: %v", err)
	}
	if !strings.Contains(string(data), "2026-08-21T03:00:00Z launch post-filing Elk push") {
		t.Fatalf("launch is not observable:\n%s", data)
	}
}

func TestElkFilingPushLaunchFailureIsLoggedAndFailOpen(t *testing.T) {
	arkDir := t.TempDir()
	p := &elkFilingPush{
		root:       "/repo",
		arkDir:     arkDir,
		executable: "/missing/ark",
		now:        func() time.Time { return time.Unix(0, 0) },
		start:      func(*exec.Cmd) error { return errors.New("process unavailable") },
	}

	// launch has no error return by design: an optional observer cannot change
	// the already-committed filing's result.
	p.launch()
	data, err := os.ReadFile(filepath.Join(arkDir, elkPushLogName))
	if err != nil {
		t.Fatalf("read push log: %v", err)
	}
	if !strings.Contains(string(data), "launch failed: process unavailable") {
		t.Fatalf("failure is not observable:\n%s", data)
	}
}
