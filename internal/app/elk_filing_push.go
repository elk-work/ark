package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const elkPushLogName = "elk-push.log"

type elkFilingPush struct {
	root       string
	arkDir     string
	executable string
	start      func(*exec.Cmd) error
	now        func() time.Time
	debug      func(format string, args ...any)
}

// newElkFilingPush enables the post-filing observer only when the existing
// manual push configuration is complete. Credentials stay in the environment:
// they are never copied into repository config, process arguments, or logs.
func newElkFilingPush(root, arkDir string, debug func(format string, args ...any)) *elkFilingPush {
	if os.Getenv("ARK_ELK_ENDPOINT") == "" || os.Getenv("ARK_ELK_TOKEN") == "" {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		if debug != nil {
			debug("Elk per-filing push disabled: locate ark executable: %v", err)
		}
		return nil
	}
	return &elkFilingPush{
		root:       root,
		arkDir:     arkDir,
		executable: executable,
		start:      startDetached,
		now:        time.Now,
		debug:      debug,
	}
}

// launch starts the existing replay-safe `ark elk push` path after a mutation
// commits. It waits only for the OS to start the child process, never for Elk
// or for delivery to finish. Every failure is fail-open and best-effort output
// is appended to ignored local Ark state for inspection.
func (p *elkFilingPush) launch() {
	logPath := filepath.Join(p.arkDir, elkPushLogName)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		p.report("open %s: %v", logPath, err)
		return
	}
	defer log.Close()

	_, _ = fmt.Fprintf(log, "%s launch post-filing Elk push\n", p.now().UTC().Format(time.RFC3339Nano))
	cmd := exec.Command(p.executable, "--json", "-C", p.root, "elk", "push")
	cmd.Stdout = log
	cmd.Stderr = log
	if err := p.start(cmd); err != nil {
		_, _ = fmt.Fprintf(log, "%s launch failed: %v\n", p.now().UTC().Format(time.RFC3339Nano), err)
		p.report("start post-filing Elk push: %v", err)
	}
}

func (p *elkFilingPush) report(format string, args ...any) {
	if p.debug != nil {
		p.debug(format, args...)
	}
}

// startDetached releases the child immediately after start. The child inherits
// ARK_ELK_ENDPOINT and ARK_ELK_TOKEN, and owns its duplicated log descriptor;
// the filing process neither waits for nor observes the network request.
func startDetached(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
