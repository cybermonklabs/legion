package oauthstore_test

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
)

// TestMain runs the package's tests through storetest.Main, which drops the test binary's own
// database that storetest.URL makes each test's schema in. It is in the external test package, as
// every test that reads storetest is: storetest imports store, which imports this package
// (store.RevokePersonSignInTx reads NarrowedLive), so the package's own tests cannot import it.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }
