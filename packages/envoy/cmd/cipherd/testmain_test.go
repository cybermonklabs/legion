package main

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestMain removes the binaries the package's tests built (cmdtest.Run), and drops the test
// binary's own database that storetest.URL makes each test's schema in (storetest.Main).
func TestMain(m *testing.M) { os.Exit(cmdtest.Run(func() int { return storetest.Main(m) })) }
