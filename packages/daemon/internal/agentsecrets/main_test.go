package agentsecrets

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/brokertest"
)

// TestMain drops the databases the broker rigs clone (brokertest.Main).
func TestMain(m *testing.M) { os.Exit(brokertest.Main(m.Run)) }
