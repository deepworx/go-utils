package inbox_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/deepworx/go-utils/internal/pgtest"
	"github.com/deepworx/go-utils/pkg/inbox"
	"github.com/deepworx/go-utils/pkg/postgres/notify"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

func TestNewStoreRejectsSchemaWithInvalidChannel(t *testing.T) {
	t.Parallel()

	_, err := inbox.NewStore(nil, inbox.StoreConfig{Schema: strings.Repeat("s", 58)})
	if !errors.Is(err, notify.ErrInvalidChannel) {
		t.Errorf("NewStore() error = %v, want notify.ErrInvalidChannel", err)
	}
}

func TestNewStoreRejectsNilPool(t *testing.T) {
	t.Parallel()

	if _, err := inbox.NewStore(nil, inbox.StoreConfig{}); !errors.Is(err, inbox.ErrInvalidConfig) {
		t.Errorf("NewStore() error = %v, want ErrInvalidConfig", err)
	}
}

func TestInsertAcceptsKeyOfMaxLen(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	msg := inbox.Message{Source: "s", Key: strings.Repeat("k", inbox.MaxKeyLen), Type: "t"}
	if inserted, err := store.Insert(t.Context(), msg); err != nil || !inserted {
		t.Fatalf("Insert() = %v, %v, want true, nil", inserted, err)
	}
}

func TestInsertRejectsIncompleteMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  inbox.Message
	}{
		{name: "empty source", msg: inbox.Message{Key: "k", Type: "t"}},
		{name: "empty key", msg: inbox.Message{Source: "s", Type: "t"}},
		{name: "empty type", msg: inbox.Message{Source: "s", Key: "k"}},
		{name: "key too long", msg: inbox.Message{Source: "s", Key: strings.Repeat("k", inbox.MaxKeyLen+1), Type: "t"}},
	}
	store, _ := newStore(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inserted, err := store.Insert(t.Context(), tt.msg)
			if !errors.Is(err, inbox.ErrInvalidMessage) {
				t.Fatalf("Insert() error = %v, want ErrInvalidMessage", err)
			}
			if inserted {
				t.Error("Insert() inserted = true, want false")
			}
		})
	}
}

func TestInsertDeduplicatesPerSourceTenantAndKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		first        inbox.Message
		second       inbox.Message
		cleanup      bool
		wantInserted bool
	}{
		{
			name:         "same key in another tenant",
			first:        inbox.Message{Source: "src", Key: "k", Type: "t", Tenant: "a"},
			second:       inbox.Message{Source: "src", Key: "k", Type: "t", Tenant: "b"},
			wantInserted: true,
		},
		{
			name:   "same key in the same tenant",
			first:  inbox.Message{Source: "src", Key: "k", Type: "t", Tenant: "a"},
			second: inbox.Message{Source: "src", Key: "k", Type: "t", Tenant: "a"},
		},
		{
			name:   "same key without tenant",
			first:  inbox.Message{Source: "src", Key: "k", Type: "t"},
			second: inbox.Message{Source: "src", Key: "k", Type: "t"},
		},
		{
			name:         "same key with and without tenant",
			first:        inbox.Message{Source: "src", Key: "k", Type: "t"},
			second:       inbox.Message{Source: "src", Key: "k", Type: "t", Tenant: "a"},
			wantInserted: true,
		},
		{
			name:         "same key in another source",
			first:        inbox.Message{Source: "src", Key: "k", Type: "t", Tenant: "a"},
			second:       inbox.Message{Source: "other", Key: "k", Type: "t", Tenant: "a"},
			wantInserted: true,
		},
		{
			name:         "same key after cleanup",
			first:        inbox.Message{Source: "src", Key: "k", Type: "t", Tenant: "a"},
			second:       inbox.Message{Source: "src", Key: "k", Type: "t", Tenant: "a"},
			cleanup:      true,
			wantInserted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store, _ := newStore(t)
			mustInsert(t, store, tt.first)
			if tt.cleanup {
				drain(t, newWorker(t, store, inbox.Config{
					Source: tt.first.Source,
					Handle: func(context.Context, inbox.Delivery) error { return nil },
				}))
				if _, err := store.Cleanup(t.Context(), time.Now().Add(time.Hour)); err != nil {
					t.Fatalf("Cleanup() error = %v", err)
				}
			}

			inserted, err := store.Insert(t.Context(), tt.second)
			if err != nil {
				t.Fatalf("Insert() error = %v", err)
			}
			if inserted != tt.wantInserted {
				t.Errorf("Insert() inserted = %v, want %v", inserted, tt.wantInserted)
			}
		})
	}
}
