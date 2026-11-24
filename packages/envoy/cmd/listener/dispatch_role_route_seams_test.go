package main

import dispatchapi "github.com/sjawhar/envoy/internal/dispatch/api"

// useDispatchPrincipal names principal as Dispatch's on d, as main.go sets it from
// ENVOY_DISPATCH_PRINCIPAL: the one verified principal whose sends may carry a role mark.
func useDispatchPrincipal(d *listenerDeps, principal string) {
	d.dispatchPrincipal = principal
}

// presentAccountToken has Dispatch present its own identity to the listener, the projected token
// tokenFile holds when each call is made, as cmd/dispatch sets it from ENVOY_ACCOUNT_TOKEN_FILE.
func presentAccountToken(input *dispatchapi.DepsInput, tokenFile string) {
	input.EnvoyAccountTokenFile = tokenFile
}
