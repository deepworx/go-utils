// Package storehook lets eventstore hand the read side of its stores to the
// consumer runtimes without exporting it.
//
// eventstore registers its lookups at init time. A runtime imports
// eventstore, so the lookups are always registered before they are used.
// The returned values have exported methods on unexported types; the
// runtime asserts them to its own unexported port interfaces.
package storehook

var inMemoryFeed func(store any) (any, bool)

// RegisterInMemoryFeed installs the lookup from an eventstore InMemoryStore
// to its feed. Only package eventstore calls it.
func RegisterInMemoryFeed(lookup func(store any) (any, bool)) {
	inMemoryFeed = lookup
}

// InMemoryFeed returns the feed of an eventstore InMemoryStore, or false
// when store is none.
func InMemoryFeed(store any) (any, bool) {
	if inMemoryFeed == nil {
		return nil, false
	}
	return inMemoryFeed(store)
}

var pgStore func(store any) (any, bool)

// RegisterPgStore installs the lookup from an eventstore PgStore to its
// read-side settings. Only package eventstore calls it.
func RegisterPgStore(lookup func(store any) (any, bool)) {
	pgStore = lookup
}

// PgStore returns the read-side settings of an eventstore PgStore, or false
// when store is none.
func PgStore(store any) (any, bool) {
	if pgStore == nil {
		return nil, false
	}
	return pgStore(store)
}
