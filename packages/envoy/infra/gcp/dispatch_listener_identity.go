package gcp

// Dispatch's own identity at its listener: the app's projected token, and the part of the
// listener's environment that verifies it and takes a send's role mark from it alone.

import (
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
)

// dispatchServiceAccountSubject is the subject of the app's projected service-account tokens,
// system:serviceaccount:<namespace>:<hostname>: ServiceAccount <hostname>, which only the app's
// pods run as. The serviceAccountSubject output names it, and so does the listener's
// ENVOY_DISPATCH_PRINCIPAL.
func dispatchServiceAccountSubject(s dispatchSettings) string {
	return "system:serviceaccount:" + s.namespace + ":" + s.hostname
}

// dispatchAccountTokenDir is where envoy-dispatch mounts its projected token for the listener's
// audience, and dispatchAccountTokenFile the file ENVOY_ACCOUNT_TOKEN_FILE names in it. The
// kubelet mints the token for dispatchAccountTokenSeconds, an hour, and rewrites it once it is 80%
// of the way through that life, so the file always holds a token with twelve minutes or more to
// run, which Dispatch reads at each listener call.
const (
	dispatchAccountTokenDir     = "/var/run/secrets/envoy-listener"
	dispatchAccountTokenFile    = dispatchAccountTokenDir + "/token"
	dispatchAccountTokenSeconds = 3600
)

// dispatchListenerIdentityEnv is the part of the listener's environment that admits Dispatch's own
// identity: the projected-token verifier of this cluster's issuer for audience, the listener's own
// URL, held to the app's account alone (ENVOY_OIDC_ACCOUNTS), which is also the one principal the
// listener takes a send's role mark from (ENVOY_DISPATCH_PRINCIPAL). Only the app's pods run as
// that account: the listener runs as <hostname>-listener and the front door as <hostname>-tailnet,
// so no other pod of the namespace can project a token naming it. The audience is the listener's
// URL, so a token minted for it is presented to this listener alone.
func dispatchListenerIdentityEnv(s dispatchSettings, issuer, audience string) corev1.EnvVarArray {
	subject := dispatchServiceAccountSubject(s)
	return corev1.EnvVarArray{
		valueEnv("ENVOY_OIDC_ISSUER", issuer), valueEnv("ENVOY_OIDC_AUDIENCE", audience),
		valueEnv("ENVOY_OIDC_ACCOUNTS", subject), valueEnv("ENVOY_DISPATCH_PRINCIPAL", subject),
	}
}
