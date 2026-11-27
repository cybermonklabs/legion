package ciphertest

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestMain runs the package's tests through storetest.Main, which drops the test binary's own
// database that storetest.URL makes each test's schema in.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }
