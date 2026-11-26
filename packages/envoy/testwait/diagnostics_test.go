package testwait

import (
	"strings"
	"testing"
	"time"
)

type sharedNode struct {
	left, right *sharedNode
}

type firstPointer struct {
	first any
}

// A 30-level graph whose two pointers share every next node stays under the cap, and a pointer to
// a struct and a pointer to its first field do not become a false cycle because place carries type.
func TestDescribeBoundsSharedPointersAndKeysCyclesByType(t *testing.T) {
	leaf := &sharedNode{}
	for range 30 {
		leaf = &sharedNode{left: leaf, right: leaf}
	}
	if got := Describe(leaf); len(got) > descriptionLimit {
		t.Errorf("the 30-level shared reading is %d bytes, more than the %d-byte cap", len(got), descriptionLimit)
	}
	value := &firstPointer{}
	value.first = &value.first
	got := Describe(value)
	if strings.HasPrefix(got, "&{first:<cycle>") {
		t.Errorf("a pointer to a struct and a pointer to its first field, which share an address but not a type, printed a false cycle: %s", got)
	}
	if len(got) > descriptionLimit {
		t.Errorf("the cyclic reading is %d bytes, more than the %d-byte cap", len(got), descriptionLimit)
	}
}

// Collateral diagnostics name the longest completed wait recorded before them, and a wait that has
// not begun by its end calls no unrelated testwait wait its cause.
func TestDiagnosticsNamesTheLongestCompletedWait(t *testing.T) {
	ResetForTest()
	t.Cleanup(ResetForTest)
	now := time.Now()
	Diagnostics(t, Wait{What: "a short wait", Began: now.Add(-time.Second), End: now.Add(-time.Millisecond), EndDescription: "the caller's end"})
	Diagnostics(t, Wait{What: "the long wait", Began: now.Add(-5 * time.Second), End: now.Add(-time.Millisecond), EndDescription: "the caller's end"})
	got := Diagnostics(t, Wait{What: "a late wait", Began: now, End: now.Add(-time.Second), Spent: true, EndDescription: "the caller's end"})
	if !strings.Contains(got, "longest completed testwait give-up recorded so far waited") || !strings.Contains(got, "waiting for the long wait") {
		t.Errorf("the collateral diagnostic names no longest completed wait:\n%s", got)
	}
	ResetForTest()
	got = Diagnostics(t, Wait{What: "a late wait", Began: now, End: now.Add(-time.Second), Spent: true, EndDescription: "the caller's end"})
	if !strings.Contains(got, "no completed testwait give-up is recorded yet") {
		t.Errorf("a collateral diagnostic with no completed wait makes a false causal claim:\n%s", got)
	}
}
