//go:build natslive

package main

import "github.com/sjawhar/envoy/internal/cipher/store/storetest"

// The relay socket rows start a broker (ciphertest.NewRig), which makes its schema in the test
// binary's own database (storetest.URL), so under natslive the package's tests run inside
// storetest.Run, which then drops that database (testmain_test.go's testMainHarness).
func init() { testMainHarness = storetest.Run }
