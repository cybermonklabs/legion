package main

import (
	"github.com/sjawhar/envoy/internal/dispatch/routefile"
)

// routeFileName is the name of the file that declares one route in package api. The rule, and the
// test that holds every route file to it, are routefile's (internal/dispatch/routefile).
func routeFileName(method, pattern string) (string, error) {
	return routefile.Name(method, pattern)
}

// isRouteFile reports whether a file name is one routeFileName writes.
func isRouteFile(name string) bool {
	return routefile.Is(name)
}
