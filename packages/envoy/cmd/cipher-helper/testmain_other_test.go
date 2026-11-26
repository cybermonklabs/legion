// packages/envoy/cmd/cipher-helper/testmain_other_test.go
//go:build !linux

package main

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// TestMain is main_test.go's off linux, where no test starts this binary as a session to register
// with the helper: version_test.go's builds still need cmdtest.Main.
func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }
