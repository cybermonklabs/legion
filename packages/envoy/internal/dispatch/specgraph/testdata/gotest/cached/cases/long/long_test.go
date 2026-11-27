// Package long has one test, which passes. make-reports.sh moves it to an import path of 1,133
// bytes, longer than the 1,024 bytes at which go test's converter cuts an output event, so the
// package's summary line comes in two events.
package long

import "testing"

func TestA(t *testing.T) {}
