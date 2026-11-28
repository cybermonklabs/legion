package store

import (
	"strconv"
	"time"
)

// StatementTimeout is d as a statement_timeout holds it: whole milliseconds, never under one,
// since a statement_timeout of 0 turns the bound off. A caller that names the bound in force -
// in a message, say - names this rather than d.
func StatementTimeout(d time.Duration) time.Duration {
	return time.Duration(max(d.Milliseconds(), 1)) * time.Millisecond
}

// StatementTimeoutSetting is the `set local` that bounds each later statement of a transaction
// to d, as StatementTimeout holds it, written as a bare number of milliseconds. Not d.String():
// Go writes a minute or more as "1m0s" and less than a millisecond as "500µs", and Postgres
// refuses both for statement_timeout (invalid value for parameter "statement_timeout").
func StatementTimeoutSetting(d time.Duration) string {
	return "set local statement_timeout = " + strconv.FormatInt(StatementTimeout(d).Milliseconds(), 10)
}
