// Package deliverytest seeds deliveryseed's delivery history into a test's database. It is an
// ordinary package rather than a _test.go file so that every package whose tests need such a
// history can import it (Go cannot share test-only code across packages), and only test files
// import it (internal/textcheck). A command writes the same history through deliveryseed.Write,
// the one writer of it, which Seed calls too.
package deliverytest

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Seed writes the history into the database (deliveryseed.Write), fails the test if it cannot,
// logs one line naming what it wrote, and answers it.
func Seed(t testing.TB, pool *store.Pool, spec deliveryseed.Spec) deliveryseed.Fixture {
	t.Helper()
	started := time.Now()
	fixture, err := deliveryseed.Write(t.Context(), pool, spec)
	if err != nil {
		t.Fatalf("seed the delivery history: %v", err)
	}
	t.Logf("%s; written in %s", fixture.Summary(), time.Since(started).Round(time.Millisecond))
	return fixture
}
