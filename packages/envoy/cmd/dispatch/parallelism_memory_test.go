//go:build memory

package main

// The memory build keeps go test's default -parallel, so it sets dispatchTestParallelism, which
// TestMain reads, to zero (main_test.go says why). It does so in an init function, so that this
// file's constraint is the bare tag, which go-tagged-tests.sh reads.
func init() { dispatchTestParallelism = 0 }
