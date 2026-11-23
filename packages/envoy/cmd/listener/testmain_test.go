//go:build !natslive

package main

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain removes the NATS server the package's tests share (testnats.Main). Under the natslive
// tag, whose tests start a broker, testmain_natslive_test.go's TestMain runs in its place.
func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }
