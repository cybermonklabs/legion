package testwait

import shared "github.com/sjawhar/envoy/testwait"

// describe is how this module's wait prints its last reading. The shared package prints only the
// snapshot the caller handed it, never follows a pointer into a live object (shared.Describe).
func describe(value any) string { return shared.Describe(value) }
