// Package connectors maps capability slugs to their built-in handlers.
//
// v1 ships connectors compiled into the binary — no plugins, no sidecars.
// The capability slug is matched byte-for-byte (conduit's canonical-slug-match
// pin: no normalization, no lowercasing).
package connectors

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/wyre-technology/conduit-connector/internal/connectors/echo"
)

// Handle dispatches one inbound tunnel request to the built-in connector for
// its target slug.
func Handle(_ context.Context, target string, payload json.RawMessage) (json.RawMessage, error) {
	switch target {
	case "echo":
		return echo.Handle(payload)
	default:
		return nil, fmt.Errorf("no built-in connector for capability %q", target)
	}
}

// Available lists the capability slugs this binary can serve. Boot refuses a
// configured capability with no matching connector (fail-loud, never a silent
// no-op capability).
func Available() []string {
	return []string{"echo"}
}
