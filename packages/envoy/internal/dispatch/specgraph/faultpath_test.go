package specgraph

import "testing"

// The failing example of a program check of the code counts only when each path of its fault is a
// source file and no file of a test (CheckStates, step 7). A source file ends in .go, .ts or .tsx.
// A file of a test has a name that ends in _test.go, .test.ts, .test.tsx or .e2e.ts, or a directory
// in its path that starts with test, ends with test or tests, or is __tests__ or e2e. Each row is one
// part of the rule, or a path beside it that the part does not take.
func TestAFaultPathIsASourceFileAndNoFileOfATest(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		// a source file
		"packages/envoy/cmd/listener/main.go":     true,
		"packages/dispatch/web/src/client.ts":     true,
		"packages/dispatch/web/src/view.tsx":      true,
		"packages/dispatch/web/src/types.d.ts":    true,
		"main.go":                                 true,
		"packages/envoy/cmd/listener/testing.go":  true, // a name, not a directory, that starts with test
		"packages/envoy/cmd/listener/contest.go":  true, // a name, not a directory, that ends with test
		"packages/envoy/cmd/contested/main.go":    true, // a directory with test inside its name
		"packages/envoy/cmd/e2eutil/main.go":      true, // a directory that is not e2e
		"packages/dispatch/web/__tests_/view.tsx": true, // a directory that is not __tests__
		// no source file
		"packages/contracts/fixtures/dispatch-targeted-delivery.json": false,
		"docs/x.md":                            false,
		"Makefile":                             false,
		"packages/envoy/cmd/listener/go":       false,
		"packages/dispatch/web/src/a.tsx.orig": false,
		// a file of a test by its name
		"packages/envoy/cmd/listener/restart_test.go": false,
		"packages/dispatch/web/src/client.test.ts":    false,
		"packages/dispatch/web/src/view.test.tsx":     false,
		"packages/dispatch/web/flows/sign-in.e2e.ts":  false,
		// a file of a test by a directory of its path
		"packages/envoy/cmd/listener/testdata/fault.go": false,
		"packages/envoy/internal/testutil/clock.go":     false,
		"packages/envoy/cmd/loadtest/main.go":           false,
		"packages/envoy/cmd/listener/unittests/a.go":    false,
		"packages/dispatch/web/__tests__/view.tsx":      false,
		"packages/dispatch/e2e/sign-in.ts":              false,
		"test/main.go":                                  false,
		"tests/main.go":                                 false,
	} {
		if got := isFaultPath(path); got != want {
			t.Errorf("isFaultPath(%q) = %t, want %t", path, got, want)
		}
	}
}
