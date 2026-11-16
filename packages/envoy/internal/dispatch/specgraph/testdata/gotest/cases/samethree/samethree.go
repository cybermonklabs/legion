// Package samethree has three runs of one name that overlap: two parallel tests named TestSame, of
// package samethree and of package samethree_test, and a third run that the second makes when it
// prints a run line of its own name with the byte 0x16 before it, the marker of the testing
// package's own lines. The TestSame of package samethree ends last.
package samethree

// Printed is closed by the TestSame of package samethree_test after it prints the run line.
var Printed = make(chan struct{})
