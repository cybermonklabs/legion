//go:build !linux

package main

import (
	"fmt"
	"runtime"
)

// becomeSubreaper does nothing off Linux, where every test that would walk the descendants skips.
func becomeSubreaper() {}

// descendantsOf has no /proc to read off Linux; every test that calls it skips there first.
func descendantsOf(int) ([]int, error) {
	return nil, fmt.Errorf("the held-port wrapper's tests read Linux's /proc, and this is %s", runtime.GOOS)
}
