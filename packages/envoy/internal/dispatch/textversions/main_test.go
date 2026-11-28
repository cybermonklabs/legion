package textversions

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestMain drops the databases the package's tests clone (storetest.Main).
func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }
