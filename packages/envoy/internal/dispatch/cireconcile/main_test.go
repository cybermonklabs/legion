package cireconcile

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain runs the package's tests storetest.DefaultParallel at a time (SetDefaultParallel).
//
// storetest.RunEager makes Open's template before any test runs, so no leaf test pays for it: the
// time stays in the package's time, which go test prints beside the package.
//
// Each test has its own JetStream account on testnats.Main's shared server (testnats.URL).
// storetest.RunEager drops the cloned databases after testnats.Main shuts down that server.
//
// The binary's App key (appKeyPEM) is made here too, before any test runs, so no test's own time
// carries it. An error making it stays with the key, and fails each test that signs with it
// (fakeGitHub.client).
func TestMain(m *testing.M) {
	storetest.SetDefaultParallel()
	_, _ = appKeyPEM()
	os.Exit(storetest.RunEager(func() int { return testnats.Main(m) }))
}
