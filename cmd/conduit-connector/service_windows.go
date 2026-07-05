//go:build windows

package main

import (
	"context"
	"log/slog"

	"golang.org/x/sys/windows/svc"
)

// windowsService adapts the shared run() lifecycle to the Windows Service
// Control Manager: it starts run() in a goroutine and translates the SCM's
// Stop/Shutdown controls into a context cancel, so the connector unwinds the
// same way it does on Ctrl-C / systemd SIGTERM.
type windowsService struct {
	log  *slog.Logger
	exit int
}

func (s *windowsService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending, WaitHint: 10000}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, s.log) }()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	// Note: this handler never sends svc.Stopped itself. svc.Run emits the
	// single authoritative final Stopped status carrying the exit code we
	// return here — sending our own Stopped first would report it with a stale
	// (zero) code and mask a failure exit.
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s.log.Info("windows service stop requested")
				changes <- svc.Status{State: svc.StopPending, WaitHint: 20000}
				cancel()
				<-errCh // let run() drain the tunnel + connectors
				return false, 0
			default:
				s.log.Warn("unexpected service control", "cmd", uint(c.Cmd))
			}

		case err := <-errCh:
			// run() returned on its own — a fatal config guard (missing
			// RELAY_URL/ENROLLMENT_TOKEN) or a permanent client condition
			// (transient network drops are absorbed by the client's own
			// reconnect-with-backoff and never reach here). Return a
			// service-specific non-zero code so the SCM records the failure
			// (exit code + event log). This is a *graceful* Stopped, so the
			// installer's restart-on-crash recovery intentionally does NOT
			// relaunch it — a bad config must not loop-restart every 5s.
			if err != nil {
				s.log.Error("connector exited: " + err.Error())
				s.exit = 1
				return true, 1 // svcSpecificEC=true so the exit code is surfaced
			}
			return false, 0
		}
	}
}
