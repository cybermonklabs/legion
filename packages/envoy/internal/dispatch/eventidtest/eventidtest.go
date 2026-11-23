// Package eventidtest shares the parsed SQL reader that the Go-source and migration event-id
// guards use. It is an ordinary package because Go cannot share test-only code across packages.
//
// The guards hold the safe horizon's proof (internal/dispatch/events/horizon.go, "Safe horizon"):
// an event id is drawn only by the column default of the insert that writes its row, so the writer
// holds RowExclusiveLock on events before it holds the id.
package eventidtest
