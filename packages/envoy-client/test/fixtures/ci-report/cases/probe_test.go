// Package probe holds the four tests of the go test reports beside cases/, which make-reports.sh
// runs. A test fails when PROBE_FAILS, test names split at commas, names it, so one package gives
// each report its own failing test.
package probe

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// probe logs that the probe named name read its claim, or fails as a probe that read none when
// PROBE_FAILS names the test.
func probe(t *testing.T, name string) {
	t.Helper()
	if slices.Contains(strings.Split(os.Getenv("PROBE_FAILS"), ","), t.Name()) {
		t.Errorf("the %s read 0 claims, want 1", name)
		return
	}
	t.Logf("the %s read 1 claim", name)
}

func TestProbe(t *testing.T) { probe(t, "probe") }

func TestSecondProbe(t *testing.T) { probe(t, "second probe") }

func TestThirdProbe(t *testing.T) { probe(t, "third probe") }

func TestFourthProbe(t *testing.T) { probe(t, "fourth probe") }
