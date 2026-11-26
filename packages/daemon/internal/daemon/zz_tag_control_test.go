//go:build e2e

// CONTROL ONLY (never merge): a redeclaration of conflictBaseRules, which only the e2e build has
// (conflict_live_test.go, also //go:build e2e). An untagged `go vet ./...` excludes both this file
// and conflict_live_test.go, so it compiles and exits 0. `go vet -tags e2e ./...` includes both and
// fails: "conflictBaseRules redeclared in this block". The difference proves the tagged build is
// actually vetted, not merely named on a flag.
package daemon

func conflictBaseRules() map[string]any { return nil }
