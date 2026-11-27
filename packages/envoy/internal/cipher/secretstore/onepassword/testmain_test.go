package onepassword_test

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestMain runs the package's tests through storetest.Main, which drops the test binary's own
// database that storetest.Open makes each test's schema in: the budget's rows live in the
// broker's Postgres (migration 0052's onepassword_requests), so budget_test.go opens one, and
// storetest.Open fails a test whose package does not run through here.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }
