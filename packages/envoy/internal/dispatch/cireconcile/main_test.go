package cireconcile

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain runs the package's tests storetest.DefaultParallel at a time (SetDefaultParallel).
//
// Each test has its own JetStream account on testnats.Main's shared server (testnats.URL).
// storetest.Run drops the cloned databases after testnats.Main shuts down that server.
func TestMain(m *testing.M) {
	storetest.SetDefaultParallel()
	os.Exit(storetest.Run(func() int { return testnats.Main(m) }))
}
