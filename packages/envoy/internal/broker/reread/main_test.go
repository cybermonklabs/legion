package reread

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
)

// TestMain drops the databases the package's tests clone (storetest.Main).
func TestMain(m *testing.M) { os.Exit(storetest.Main(m.Run)) }
