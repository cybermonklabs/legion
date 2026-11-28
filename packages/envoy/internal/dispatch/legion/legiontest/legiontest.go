// Package legiontest seeds legionseed's Legion history into a test's database. It is an ordinary
// package rather than a _test.go file, as store/storetest is, so that every package whose tests
// need such a history can import it (Go cannot share test-only code across packages), and only
// test files import it (internal/textcheck). A command writes the same history through
// legionseed.Write, the one writer of it, which Seed calls too.
package legiontest

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legionseed"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Seed writes one project's Legion history into the database (legionseed.Write), fails the test
// if it cannot, logs one line naming what it wrote, and answers it.
func Seed(t testing.TB, pool *store.Pool, spec legionseed.Spec) legionseed.Fixture {
	t.Helper()
	fixture, err := legionseed.Write(t.Context(), pool, spec)
	if err != nil {
		t.Fatalf("seed the Legion history: %v", err)
	}
	t.Log(fixture.Summary())
	return fixture
}
