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

// Register makes an adapter available under name. Adapters call this from
// init (e.g. internal/store/sqlite registers "sqlite"). Duplicate names panic.
func Register(name string, open Opener) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("store: duplicate driver " + name)
	}
	registry[name] = open
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
