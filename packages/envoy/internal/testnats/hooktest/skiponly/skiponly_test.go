//go:build odatrace

// Package skiponly is a fixture of scripts/forced-order.test.sh: its one forced-order test skips
// itself, so a forced-order run of it runs no forced-order test, which the script must refuse.
package skiponly

import "testing"

func TestForcedOrderThatSkipsItself(t *testing.T) {
	t.Skip("a fixture of forced-order.test.sh: this forced-order test runs nothing")
}
