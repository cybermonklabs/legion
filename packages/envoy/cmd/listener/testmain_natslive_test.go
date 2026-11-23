//go:build natslive

package main

import (
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestMain removes the NATS server the package's tests share (testnats.Main), inside
// storetest.Run, which then drops the test binary's own database: the relay socket rows start a
// broker (ciphertest.NewRig), which makes its schema there (storetest.URL).
func TestMain(m *testing.M) {
	os.Exit(storetest.Run(func() int { return testnats.Main(m) }))
}
