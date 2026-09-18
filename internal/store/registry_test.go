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

func TestRegistryOpen(t *testing.T) {
	store.Register("stub-test", func(_ context.Context, dsn string) (store.Store, error) {
		return &stubStore{dsn: dsn}, nil
	})
	st, err := store.Open(context.Background(), "stub-test", "whatever")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := st.(*stubStore).dsn; got != "whatever" {
		t.Errorf("dsn = %q", got)
	}
}

func TestRegistryUnknownDriver(t *testing.T) {
	_, err := store.Open(context.Background(), "no-such-driver", "x")
	if err == nil || !strings.Contains(err.Error(), "unknown driver") {
		t.Errorf("expected unknown-driver error, got %v", err)
	}
}

func TestRegistryDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	store.Register("stub-test", func(_ context.Context, _ string) (store.Store, error) {
		return &stubStore{}, nil
	})
}
