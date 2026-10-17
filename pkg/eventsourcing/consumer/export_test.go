package consumer

// PgFilteredReadSQL returns the filtered read statement for schema.
func PgFilteredReadSQL(schema string) string { return newPgQueries(schema).readFiltered }
