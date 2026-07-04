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
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/wyre-technology/conduit-connector/internal/connectors"
	"github.com/wyre-technology/conduit-connector/internal/tunnel"
)

const docsBase = "https://conduit.wyre.ai/docs/guides/onprem"

func main() {
	log := newLogger()
	slog.SetDefault(log) // so connectors that log (mcp-proxy child stderr) use it

	relayURL, token, err := requireEnv()
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}

	registry := connectors.NewRegistry()
	client := tunnel.NewClient(tunnel.Options{
		RelayURL:        relayURL,
		EnrollmentToken: token,
		OnRequest:       registry.Handle,
		OnConfigUpdate:  registry.Apply,
		Logger:          log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("conduit-connector ready: dialing " + relayURL)
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("connector stopped", "error", err)
		os.Exit(1)
	}
	log.Info("connector shut down cleanly")
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

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
