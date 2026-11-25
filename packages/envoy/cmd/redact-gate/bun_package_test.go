package main

import "testing"

// TestABunFileNamesItsPackageWhateverItsDepth: a Bun row's file is run from its first two path
// elements, and a file with fewer is answered as it is, where the mutation stage's run indexed
// past the end of the split path and panicked on a file with no "/".
func TestABunFileNamesItsPackageWhateverItsDepth(t *testing.T) {
	for file, want := range map[string]string{
		"packages/contracts/src/remote-text.test.ts": "packages/contracts",
		"packages/remote-text.test.ts":               "packages/remote-text.test.ts",
		"remote-text.test.ts":                        "remote-text.test.ts",
	} {
		if got := bunPackage(file); got != want {
			t.Errorf("bunPackage(%q) = %q, want %q", file, got, want)
		}
	}
}
