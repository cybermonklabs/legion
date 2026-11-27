// Package hooktest holds the forced-order tests of the hold points themselves
// (internal/testnats/hooks.go): one test per point, each showing that the hooked nats-server
// stops at the point while a test holds it and goes on once the test releases it. Its tests carry
// the odatrace tag, so only packages/envoy/scripts/forced-order.sh builds them, and they are a
// package of their own so that its run compiles no other test of internal/testnats.
package hooktest
