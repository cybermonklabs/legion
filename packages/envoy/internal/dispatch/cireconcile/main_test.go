package cireconcile

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain runs the package's tests storetest.DefaultParallel at a time (SetDefaultParallel).
//
// storetest.RunEager makes Open's template before any test runs, so no leaf test pays for it:
// under storetest.Run the first test to call Open made it, and the package's one serial test
// (TestAPassCutShortByItsDeadlineRecordsThatItsBoundEndedIt) spent over 7 s of its 9 s in CI on
// it. The time stays in the package's time, which go test prints beside the package.
//
// Each test has its own JetStream account on testnats.Main's shared server (testnats.URL).
// storetest.RunEager drops the cloned databases after testnats.Main shuts down that server.
//
// The binary's App key (appKeyPEM) is made here too, before any test runs, for the same reason:
// the first test to sign made it, 28 to 165 ms of that test's time. An error making it stays
// with the key, and fails each test that signs with it (fakeGitHub.client).
func TestMain(m *testing.M) {
	storetest.SetDefaultParallel()
	_, _ = appKeyPEM()
	os.Exit(storetest.RunEager(func() int { return testnats.Main(m) }))
}
