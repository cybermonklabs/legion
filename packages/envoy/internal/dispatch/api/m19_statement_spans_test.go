package api

// The scanner of the statement-binding rule (m19_statement_binding_test.go): where each SQL
// fragment of one statement begins and ends, read as Postgres's scanner reads it, so a marker
// inside a string, a quoted name or a dollar-quoted body is no marker.
