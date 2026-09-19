package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Opener builds a Store from an adapter-specific connection string.
type Opener func(ctx context.Context, dsn string) (Store, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Opener{}
)

// Register makes an adapter available under name. Adapters usually call
// MustRegister from init; direct callers get an error on duplicates
// instead of a panicking binary.
func Register(name string, open Opener) error {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		return fmt.Errorf("store: duplicate driver %q", name)
	}
	registry[name] = open
	return nil
}

// MustRegister is Register for init-time callers: it panics on duplicate
// names, preserving fail-fast wiring for adapters.
func MustRegister(name string, open Opener) {
	if err := Register(name, open); err != nil {
		panic(err.Error())
	}
}

// ResetForTest clears the adapter registry. It is a test seam for adapter
// tests that must control registration order: callers own the registry
// afterwards and must re-register whatever they need.
func ResetForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Opener{}
}

// Open builds whatever adapter driver names, purely through the Store port.
// Unknown drivers fail with the list of registered names — never a nil store.
func Open(ctx context.Context, driver, dsn string) (Store, error) {
	registryMu.RLock()
	open, ok := registry[driver]
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	registryMu.RUnlock()
	if !ok {
		sort.Strings(names)
		return nil, fmt.Errorf("store: unknown driver %q (registered: %v)", driver, names)
	}
	st, err := open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", driver, err)
	}
	return st, nil
}
