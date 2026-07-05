// conduit-connector — the Conduit on-prem connector agent.
//
// Dials OUT over WSS to the Conduit relay; binds no inbound port. Boot
// guards mirror conduit's src/onprem/index.ts six-guard discipline: any
// guard failure refuses to boot with a named, actionable error.
//
// Protocol v2 (conduit docs/onprem-connector-v1.md): enrollment is
// IDENTITY-ONLY and capabilities are cloud-managed — the Conduit wizard
// pushes connector config over the tunnel (config_update) and this agent
// acks what it applied. There is no CAPABILITIES env var; the only
// site-side configuration is WHERE to dial and WHO this site is:
//
//	RELAY_URL         wss:// relay endpoint (production: wss://conduit-wss.wyre.ai)
//	ENROLLMENT_TOKEN  WYRE-issued identity-only enrollment JWT
//	LOG_LEVEL         debug|info|warn|error (default info)
//
// Runtime targets: a Linux systemd service (see install.sh) and a Windows
// service (see install.ps1 + service_windows.go). On both, the process is the
// same static binary; only the service-lifecycle glue is platform-specific
// (dispatch_windows.go / dispatch_other.go). Run it in a terminal with no
// service manager and it runs interactively (Ctrl-C to stop).
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/wyre-technology/conduit-connector/internal/connectors"
	"github.com/wyre-technology/conduit-connector/internal/tunnel"
)

const docsBase = "https://conduit.wyre.ai/docs/guides/onprem"

// main hands off to the platform dispatcher, which decides between running
// interactively, running under a service manager, or handling a management
// subcommand (Windows install/uninstall). It returns a process exit code.
func main() {
	os.Exit(dispatchMain())
}

// run is the core lifecycle, shared by the interactive and service paths:
// validate config, build the tunnel client, and run until ctx is cancelled
// (Ctrl-C interactively; a Stop/Shutdown control on Windows) or the client
// exits with an error. It is the single source of truth for what the
// connector *does*, so the two entry paths cannot drift.
func run(ctx context.Context, log *slog.Logger) error {
	relayURL, token, err := requireEnv()
	if err != nil {
		return err
	}

	registry := connectors.NewRegistry()
	client := tunnel.NewClient(tunnel.Options{
		RelayURL:        relayURL,
		EnrollmentToken: token,
		OnRequest:       registry.Handle,
		OnConfigUpdate:  registry.Apply,
		Logger:          log,
	})

	log.Info("conduit-connector ready: dialing " + relayURL)
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("connector stopped: %w", err)
	}
	log.Info("connector shut down cleanly")
	return nil
}

// runInteractive runs the connector in the foreground, cancelling on Ctrl-C
// (SIGINT) or SIGTERM. This is the path for a terminal, a container, and the
// Linux systemd unit (systemd delivers SIGTERM on stop).
func runInteractive(log *slog.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), interactiveSignals()...)
	defer stop()
	return exitCode(log, run(ctx, log))
}

// exitCode logs a non-nil error and maps it to a process exit code.
func exitCode(log *slog.Logger, err error) int {
	if err != nil {
		log.Error(err.Error())
		return 1
	}
	return 0
}

func requireEnv() (relayURL, token string, err error) {
	relayURL = os.Getenv("RELAY_URL")
	if relayURL == "" {
		return "", "", fmt.Errorf(
			"FATAL: RELAY_URL env var is required. The connector refuses to start without a WYRE "+
				"relay endpoint to dial. Production: RELAY_URL=wss://conduit-wss.wyre.ai. See: %s/reference#relay-url", docsBase)
	}
	if !strings.HasPrefix(relayURL, "wss://") {
		return "", "", fmt.Errorf(
			"FATAL: RELAY_URL must be wss:// — TLS is not optional (got %q). See: %s/reference#relay-url", relayURL, docsBase)
	}
	token = os.Getenv("ENROLLMENT_TOKEN")
	if token == "" {
		return "", "", fmt.Errorf(
			"FATAL: ENROLLMENT_TOKEN env var is required. Mint one in Conduit (site → Deploy connector). "+
				"See: %s/reference#enrollment-token", docsBase)
	}
	if os.Getenv("CAPABILITIES") != "" {
		return "", "", fmt.Errorf(
			"FATAL: CAPABILITIES is not a conduit-connector setting — capabilities are configured in "+
				"Conduit and pushed to this agent automatically (protocol v2). Remove the env var. "+
				"(Deploying the legacy v1 container? That image reads CAPABILITIES; this binary does not.) "+
				"See: %s/reference#capabilities", docsBase)
	}
	return relayURL, token, nil
}

// newLogger builds the default stdout JSON logger (interactive / systemd,
// where journald captures stdout).
func newLogger() *slog.Logger { return newLoggerTo(os.Stdout) }

// newLoggerTo builds a JSON logger writing to w at the env-selected level.
// The Windows service path uses this to log to a file, since a service has no
// console to capture stdout.
func newLoggerTo(w io.Writer) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
