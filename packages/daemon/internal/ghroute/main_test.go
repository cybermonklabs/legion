package ghroute

import (
	"os"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/programbound"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// TestMain gives every bound registered with programbound, this package's help read's
// (helpTimeout) and git reads' (GitReadBound) among them, what is left of this test binary's
// deadline (testwait.BinaryDeadline) less 30 s when its wait starts, as cmd/legion's TestMain does
// for `legion gh`: the help-read rows run ReadHelp on stand-in gh scripts, and one whose read a
// loaded machine made outlast the product's 30 s would read that stop in place of the one the row
// makes. A program that never answers still fails its row by name before go test's alarm. Under
// -timeout 0, which gives no deadline, the product's bounds hold.
func TestMain(m *testing.M) {
	if deadline, ok := testwait.BinaryDeadline(); ok {
		programbound.SetDeadline(deadline)
	}
	os.Exit(m.Run())
}
