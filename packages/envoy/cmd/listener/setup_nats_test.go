package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
)

// The bus client setupTestNATS hands a test is closed when that test ends. A bus client
// reconnects forever, so one left open goes on dialing its server's address for the rest of the
// test binary, and reaches whatever server the kernel gives that port next, in this binary or in
// another one go test runs beside it.
func TestSetupTestNATSClosesItsClientWhenItsTestEnds(t *testing.T) {
	t.Parallel()
	var client *bus.Client
	t.Run("a test that takes the client", func(t *testing.T) {
		client = setupTestNATS(t)
		if !client.Connected() {
			t.Fatal("setupTestNATS handed back a client that is not connected")
		}
	})
	if client == nil {
		t.Fatal("UNKNOWN: the subtest took no client, so there is nothing to check")
	}
	if !client.Conn.IsClosed() {
		client.Close()
		t.Fatal("setupTestNATS left its client open after its test ended")
	}
}
