package store

// LargeArgumentBytesForTest is largeArgumentBytes: the size of a statement's counted arguments
// past which its connection gives them back when it returns to its pool (argumentReleaser).
const LargeArgumentBytesForTest = largeArgumentBytes

// CountedArgumentBytesForTest is how many of arg's bytes argumentReleaser counts toward
// largeArgumentBytes (knownArgumentBytes), 0 for a kind it does not count. A test in package api
// reads every large write's arguments with it, so it holds those writes to the kinds the release
// counts and not to a copy of that list.
func CountedArgumentBytesForTest(arg any) int {
	counted, _ := knownArgumentBytes(arg, 0)
	return counted
}
