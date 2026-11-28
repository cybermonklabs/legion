// Package output holds the two tests of go-test-long-output.jsonl, which make-report.sh runs:
// each prints a start marker, a long body and an end marker, then fails.
package output

import (
	"fmt"
	"strings"
	"testing"
)

// TestThreeK prints 3000 "k" on one line between its markers.
func TestThreeK(t *testing.T) {
	fmt.Println("START-MARKER of TestThreeK")
	fmt.Println(strings.Repeat("k", 3000))
	fmt.Println("END-MARKER of TestThreeK")
	t.Errorf("TestThreeK planted failure after 3000 characters")
}

// TestSeventyK prints 70 lines of 1023 "s", 70 KiB, between its markers.
func TestSeventyK(t *testing.T) {
	fmt.Println("START-MARKER of TestSeventyK")
	for range 70 {
		fmt.Println(strings.Repeat("s", 1023))
	}
	fmt.Println("END-MARKER of TestSeventyK")
	t.Errorf("TestSeventyK planted failure after 70 KiB")
}
