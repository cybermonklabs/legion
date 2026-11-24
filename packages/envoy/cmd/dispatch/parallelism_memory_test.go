//go:build memory

package main

// dispatchTestParallelism is zero in the memory build, which leaves go test's default -parallel
// (parallelism_untagged_test.go says why).
const dispatchTestParallelism = 0
