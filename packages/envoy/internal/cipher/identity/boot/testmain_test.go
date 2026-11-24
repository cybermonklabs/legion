package boot_test

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestMain runs the package's tests through storetest.Main, which drops the test binary's own
// database that storetest.Open makes each test's schema in: stored_principals_test.go reads the
// principals and subjects a broker stored, and storetest.Open fails a test whose package does not
// run through here.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }
