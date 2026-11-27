package cireconcile

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain drops the databases the harness clones (storetest.Run) and shuts down the one NATS
// server its tests share (testnats.Main), each test under a JetStream account of its own
// (testnats.URL). Both run the package's tests themselves, so they compose the way storetest.Run
// documents. Every harness used to start a server of its own; that was a JetStream start per test.
func TestMain(m *testing.M) {
	os.Exit(storetest.Run(func() int { return testnats.Main(m) }))
}
