//go:build race

package pmdoc

// raceBuild reports whether this build has the race detector in it, which multiplies the CPU time
// every call spends (cpuCeilingPerCharacter, heavy_bodies_test.go).
const raceBuild = true
