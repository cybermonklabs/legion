//go:build !memory

package main

// dispatchTestParallelism is how many of this package's parallel tests run at once when the
// command line names no -parallel (TestMain). Most of them wait - on a Dispatch process they
// started, on Postgres, on a product timer - rather than compute: serially the test binary and its
// processes kept about a tenth of a core busy (41 CPU-s over 376 s, 4 CPUs, build pod). go test's
// default, GOMAXPROCS, is four on CI's runner; eight lets the tests that start no process run
// beside the ones that hold the four process slots (takeProcessSlot).
//
// The memory build keeps go test's default (parallelism_memory_test.go): its tests measure
// processes' memory against a runner's 16 GB and were sized at that default.
const dispatchTestParallelism = 8
