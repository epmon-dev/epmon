package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/epmon-dev/epmon/internal/store"
)

type stubStore struct {
	store.Store
	dsn string
}

func stubOpener(dsn string) store.Opener {
	return func(_ context.Context, got string) (store.Store, error) {
		return &stubStore{dsn: got}, nil
	}
}

// TestRegistryLifecycle covers the full adapter-author flow against an
// isolated registry: register, duplicate (error, not panic), open, and
// open-unknown.
func TestRegistryLifecycle(t *testing.T) {
	store.ResetForTest()
	defer store.ResetForTest()

	if err := store.Register("stub-test", stubOpener("")); err != nil {
		t.Fatalf("Register = %v, want nil", err)
	}
	if err := store.Register("stub-test", stubOpener("")); err == nil ||
		!strings.Contains(err.Error(), "duplicate driver") {
		t.Errorf("duplicate Register = %v, want duplicate-driver error", err)
	}
	st, err := store.Open(context.Background(), "stub-test", "whatever")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := st.(*stubStore).dsn; got != "whatever" {
		t.Errorf("dsn = %q, want whatever", got)
	}
	if _, err := store.Open(context.Background(), "no-such-driver", "x"); err == nil ||
		!strings.Contains(err.Error(), "unknown driver") {
		t.Errorf("Open unknown = %v, want unknown-driver error", err)
	}
}

// TestMustRegisterPanics locks in the fail-fast init-time behavior.
func TestMustRegisterPanics(t *testing.T) {
	store.ResetForTest()
	defer store.ResetForTest()

	store.MustRegister("stub-test", stubOpener(""))
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate MustRegister")
		}
	}()
	store.MustRegister("stub-test", stubOpener(""))
}
