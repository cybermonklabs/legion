package api

import "testing"

// A caller of the guard count by its old name, which this branch renamed: a vet that compiles this
// package's tests must refuse it. Scratch control only, never merged.
func TestCallsTheOldCountName(t *testing.T) { _ = goRuleCopies("x") }
