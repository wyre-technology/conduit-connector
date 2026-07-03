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
// (the gateway's reconciler re-pushes stored config; a local encrypted cache
// is a later optimization, not a correctness requirement).
package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/wyre-technology/conduit-connector/internal/connectors/echo"
	"github.com/wyre-technology/conduit-connector/internal/connectors/mssql"
	"github.com/wyre-technology/conduit-connector/internal/connectors/mysql"
	"github.com/wyre-technology/conduit-connector/internal/connectors/postgres"
)

// Handler serves one inbound tunnel request for an enabled connector.
type Handler func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error)

// factory builds a configured connector instance from its pushed config;
// returning an error keeps the connector disabled and surfaces in the
// config_ack error.
type factory func(config json.RawMessage) (Handler, error)

var builtins = map[string]factory{
	"echo": func(json.RawMessage) (Handler, error) { // echo takes no config
		return func(_ context.Context, payload json.RawMessage) (json.RawMessage, error) {
			return echo.Handle(payload)
		}, nil
	},
	"mssql": func(cfg json.RawMessage) (Handler, error) {
		c, err := mssql.New(cfg)
		if err != nil {
			return nil, err
		}
		return c.Handle, nil
	},
	"postgres": func(cfg json.RawMessage) (Handler, error) {
		c, err := postgres.New(cfg)
		if err != nil {
			return nil, err
		}
		return c.Handle, nil
	},
	"mysql": func(cfg json.RawMessage) (Handler, error) {
		c, err := mysql.New(cfg)
		if err != nil {
			return nil, err
		}
		return c.Handle, nil
	},
}

// Registry is the config-driven connector state.
type Registry struct {
	mu      sync.RWMutex
	enabled map[string]Handler
}

func NewRegistry() *Registry {
	return &Registry{enabled: map[string]Handler{}}
}

// Apply replaces the enabled set from a cloud-pushed config_update. Returns
// the applied slugs (⊆ pushed) and the FIRST failure as error (remaining
// valid connectors still apply — partial application is reported honestly:
// applied lists what serves, error names what didn't and why). configVersion
// is carried for logging/idempotency by the caller; application itself is
// version-agnostic (a re-push of the same config re-applies identically).
func (r *Registry) Apply(_ context.Context, _ int, connectors map[string]json.RawMessage) ([]string, error) {
	next := map[string]Handler{}
	applied := make([]string, 0, len(connectors))
	var firstErr error
	for slug, cfg := range connectors {
		build, ok := builtins[slug]
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("no built-in connector for %q in this binary version", slug)
			}
			continue
		}
		handler, err := build(cfg)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("connector %q rejected its config: %w", slug, err)
			}
			continue
		}
		next[slug] = handler
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
	handler, ok := r.enabled[target]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("capability %q is not enabled by the current config", target)
	}
	return handler(ctx, payload)
}
