package consumer

// PgFilteredReadSQL returns the filtered read statement for schema.
func PgFilteredReadSQL(schema string) string { return newPgQueries(schema).readFiltered }

// RestartInMemory returns a new InMemoryRuntime over the Store and Cursors
// of rt, as after a process restart. rt must no longer be used.
func RestartInMemory(rt *InMemoryRuntime) *InMemoryRuntime { return rt.restart() }

// PgFilterHistorySQL returns the claim-time check for events of added types
// at or below a Cursor, for schema.
func PgFilterHistorySQL(schema string) string { return newPgQueries(schema).historyFiltered }
