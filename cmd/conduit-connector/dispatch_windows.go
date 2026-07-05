//go:build windows

package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/svc"
)

// serviceName is the Windows service name install.ps1 registers this binary
// under. Keep it in lockstep with install.ps1.
const serviceName = "conduit-connector"

// dispatchMain (Windows): if the process was started by the Service Control
// Manager, run under the SCM control handler (service_windows.go); otherwise
// run interactively (a terminal). Service *management* — create/remove/start/
// stop — is owned by install.ps1, exactly as install.sh owns the systemd unit
// on Linux; the binary only needs to know how to *run* as a service.
func dispatchMain() int {
	isSvc, err := svc.IsWindowsService()
	if err != nil {
		log := newLogger()
		slog.SetDefault(log)
		log.Error("could not determine whether running as a Windows service: " + err.Error())
		return 1
	}
	if isSvc {
		return runService()
	}
	log := newLogger()
	slog.SetDefault(log)
	return runInteractive(log)
}

// interactiveSignals: on Windows a foreground run stops on Ctrl-C
// (os.Interrupt). SIGTERM has no meaningful delivery here; the service Stop
// control is the graceful-stop path when running under the SCM.
func interactiveSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

// runService logs to a file (a service has no console to capture stdout) and
// runs under the SCM control dispatcher.
func runService() int {
	log := newServiceLogger()
	slog.SetDefault(log)
	h := &windowsService{log: log}
	if err := svc.Run(serviceName, h); err != nil {
		log.Error("windows service dispatcher failed: " + err.Error())
		return 1
	}
	return h.exit
}

// newServiceLogger writes JSON logs to %ProgramData%\conduit-connector\logs\
// conduit-connector.log. If that can't be opened, it falls back to the stdout
// logger (void under the SCM, but never fatal).
func newServiceLogger() *slog.Logger {
	dir := filepath.Join(programData(), "conduit-connector", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return newLogger()
	}
	f, err := os.OpenFile(filepath.Join(dir, "conduit-connector.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return newLogger()
	}
	// f is intentionally not closed: it lives for the service's lifetime.
	return newLoggerTo(f)
}

func programData() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return pd
	}
	sysDrive := os.Getenv("SystemDrive")
	if sysDrive == "" {
		sysDrive = "C:"
	}
	return sysDrive + `\ProgramData`
}
