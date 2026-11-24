package bus

import (
	"flag"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// busTestParallelism is how many of this package's tests, and of a test's parallel subtests, run at
// once when the command line names no -parallel. A -race run of the package alone on 4 CPUs used
// 1.4% of them over its 720 s (build-cluster Job legion-ci-bus-race-profile-rcv2l, 2026-11-23): its
// tests wait on NATS, on proxies that never answer and on the bus's own request bounds, not on a
// core, so go test's default of GOMAXPROCS (4 on CI's arm runner) left them waiting four at a time.
// Every test calls t.Parallel except those that change or read back something the whole process
// shares (t.Setenv, the default logger the bus logs through, lifeCheckEvery, connectedVersion, the
// process's goroutines) or that hold a wall-clock bound close to what they wait for, which stay
// serial and run before the rest. A test that needs a server has a JetStream account of its own on
// the shared one (testnats.URL) or a server of its own.
//
// go test parses its flags inside m.Run, so a -parallel on the command line still wins.
const busTestParallelism = 8

// TestMain sets the package's default -parallel and removes the NATS server the package's tests
// share (testnats.Main).
func TestMain(m *testing.M) {
	if err := flag.Set("test.parallel", strconv.Itoa(busTestParallelism)); err != nil {
		panic("set this package's default -parallel: " + err.Error())
	}
	os.Exit(testnats.Main(m))
}
