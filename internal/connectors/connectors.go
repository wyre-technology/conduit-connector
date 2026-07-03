// Package connectors maps capability slugs to their built-in handlers and
// holds the cloud-pushed enablement state.
//
// v1 ships connectors compiled into the binary — no plugins, no sidecars.
// The capability slug is matched byte-for-byte (conduit's canonical-slug-match
// pin: no normalization, no lowercasing).
//
// Enablement is CLOUD-MANAGED (protocol v2): the registry starts empty and is
// replaced on each config_update. A connector serves requests only while
// enabled by the current config. Config is held in memory only — on restart
// the tunnel re-registers with zero capabilities and the cloud re-pushes
// (the gateway re-pushes stored config on tunnel registration; a local
// encrypted cache is a later optimization, not a correctness requirement).
package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/wyre-technology/conduit-connector/internal/connectors/echo"
)

// builtin is one compiled-in connector: validate/absorb config, handle requests.
type builtin struct {
	// configure applies per-connector config; returning an error keeps the
	// connector disabled and surfaces in the config_ack error.
	configure func(config json.RawMessage) error
	handle    func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error)
}

var builtins = map[string]builtin{
	"echo": {
		configure: func(json.RawMessage) error { return nil }, // echo takes no config
		handle: func(_ context.Context, payload json.RawMessage) (json.RawMessage, error) {
			return echo.Handle(payload)
		},
	},
}

// Registry is the config-driven connector state.
type Registry struct {
	mu      sync.RWMutex
	enabled map[string]builtin
}

func NewRegistry() *Registry {
	return &Registry{enabled: map[string]builtin{}}
}

// Apply replaces the enabled set from a cloud-pushed config_update. Returns
// the applied slugs (⊆ pushed) and the FIRST failure as error (remaining
// valid connectors still apply — partial application is reported honestly:
// applied lists what serves, error names what didn't and why). configVersion
// is carried for logging/idempotency by the caller; application itself is
// version-agnostic (a re-push of the same config re-applies identically).
func (r *Registry) Apply(_ context.Context, _ int, connectors map[string]json.RawMessage) ([]string, error) {
	next := map[string]builtin{}
	applied := make([]string, 0, len(connectors))
	var firstErr error
	for slug, cfg := range connectors {
		b, ok := builtins[slug]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("no built-in connector for %q in this binary version", slug)
			}
			continue
		}
		if err := b.configure(cfg); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("connector %q rejected its config: %w", slug, err)
			}
			continue
		}
		next[slug] = b
		applied = append(applied, slug)
	}
	r.mu.Lock()
	r.enabled = next
	r.mu.Unlock()
	return applied, firstErr
}

// Handle dispatches one inbound tunnel request to the enabled connector for
// its target slug.
func (r *Registry) Handle(ctx context.Context, target string, payload json.RawMessage) (json.RawMessage, error) {
	r.mu.RLock()
	b, ok := r.enabled[target]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("capability %q is not enabled by the current config", target)
	}
	return b.handle(ctx, payload)
}
