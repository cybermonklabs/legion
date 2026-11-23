// Package testnatsinprocess is a public re-export of internal/testnats's in-process NATS server,
// and, under the odatrace tag that packages/envoy/scripts/forced-order.sh builds with, of its
// hold points (hooks.go), so a module outside github.com/sjawhar/envoy (packages/daemon's
// forced-order tests) runs on the server, and holds it at the points, Envoy's tests do: Go's
// internal-package rule keeps that module from importing internal/testnats itself.
// internal/testnats stays the one place the server and the hooks are made.
package testnatsinprocess

import (
	"testing"

	internal "github.com/sjawhar/envoy/internal/testnats"
)

// Server is internal/testnats.Server: a nats-server running in the test process, reached through
// a relay that keeps its URL across a Stop and a Start.
type Server = internal.Server

// Start is internal/testnats.Start: a NATS server with JetStream, shut down when the test ends,
// with its client URL.
func Start(t testing.TB) (*Server, string) {
	t.Helper()
	return internal.Start(t)
}
