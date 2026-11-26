package storetest

import (
	"os"
	"testing"
)

// TestMain drops the databases this package's own tests clone (Main).
func TestMain(m *testing.M) { os.Exit(Main(m.Run)) }
