package main

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestMain removes the binaries the package's tests built (cmdtest.Main).
func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }
