// packages/envoy/cmd/cipher/secret_gcp_clock.go
//
// The clock of the metadata server's reads a store command makes for its last credential source.
package main

import "github.com/sjawhar/envoy/internal/clock"

// gcpMetadataClock is the clock that the bounds of the metadata server's reads run on, the token
// read's googleidentity.TokenTimeout in gcpMetadataToken and the email read's
// googleidentity.ProbeTimeout in gcpMetadataPrincipal: the wall clock, which only
// TestGCPSecretMetadataSourceIsPresentWhenItYieldsAToken replaces, with a fake it moves to each
// bound.
var gcpMetadataClock clock.Clock = clock.Real()
