package main

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestMain runs the package's tests through storetest.Main, which drops the test binary's own
// database that storetest.URL makes each test's schema in, inside cmdtest.Run, which removes the
// binaries they built.
func TestMain(m *testing.M) { os.Exit(cmdtest.Run(func() int { return storetest.Main(m) })) }
