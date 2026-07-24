//go:build !windows

package main

import (
	"log/slog"
	"os"
	"syscall"
)

// dispatchMain (non-Windows): there is no service-manager handoff — the
// process always runs interactively. systemd stops it with SIGTERM, which
// interactiveSignals() below catches, so `run` gets a clean context-cancel.
func dispatchMain() int {
	log := newLogger()
	slog.SetDefault(log) // so connectors that log (mcp-proxy child stderr) use it
	return runInteractive(log)
}

// interactiveSignals are the signals that cleanly stop a foreground run on
// Unix: Ctrl-C (SIGINT) and the systemd/`kill` default (SIGTERM).
func interactiveSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
