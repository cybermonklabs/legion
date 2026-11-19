package redeliver_test

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestMain drops the databases the harness clones (storetest.Main). Each harness starts a NATS
// server of its own (testnats.Start), so the package needs no testnats.Main.
func TestMain(m *testing.M) { os.Exit(storetest.Main(m.Run)) }
