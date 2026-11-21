package gcp

// The Dispatch deployment's listener container: its NATS, seed and API-token environment
// (dispatchListenerEnv), the part of it that verifies Dispatch's own identity and takes a send's
// role mark from it alone, and the app's projected token for the listener's audience.

import (
	"errors"
	"strconv"
	"strings"

	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// dispatchListenerEnv renders the envoy-listener container's environment from the one NATS URL
// once it resolves. core's Env names its own inputs — natsUrls by index and socks5Proxy, neither
// an input of gcp:Dispatch — so each refusal is mapped onto natsUrl, this component's input. In
// the SOCKS5 mode the proxy is fixed (the front door's Service, socks5Proxy), so its refusal means
// the URL names a local server, which the pod's NATS clients can never reach through the tailnet
// proxy. With natsInCluster there is no proxy, and core refuses a local URL only beside one, so a
// local URL is refused here instead, before the config is built, and both call sites refuse alike:
// the pods dial the URL themselves, and a loopback URL has them dial themselves. natsReplicas,
// streamMaxBytes and the sender limit reach no refusal of core's own, since newDispatchSettings
// bounds them more tightly (1 to bus.MaxReplicas, at least 1, and core.CheckSenderLimit) before
// anything registers, which is why every refusal that is not socks5Proxy's is natsUrl's. The NATS
// credential is nats's: the projected token's file, which core's Env names, or the seed's, which
// follows it.
// The listener serves on every address of its pod, and its API refuses to start on a non-loopback
// listen host without a bearer (packages/envoy/cmd/listener/apiauth.go), so the environment names
// the bearer in Secret listenerSecret.
func dispatchListenerEnv(s dispatchSettings, nats dispatchNATSCredential, listenerSecret, url string) (corev1.EnvVarArray, error) {
	if s.natsInCluster && bus.LocalNATSURL(url) {
		return nil, core.InputErrorf("natsUrl", "natsUrl names a local NATS server; with natsInCluster the pods dial it directly, so the URL must name the hub's in-cluster Service")
	}
	env, err := (core.ListenerConfig{
		MachineID:               s.hostname,
		Port:                    core.DefaultListenerPort,
		ListenHost:              "0.0.0.0",
		SOCKS5Proxy:             s.socks5Proxy(),
		NATSReplicas:            s.natsReplicas,
		StreamMaxBytes:          s.streamMaxBytes,
		SenderMessagesPerMinute: s.senderMessagesPerMinute,
		SenderMessageBurst:      s.senderMessageBurst,
		ServiceTokenFile:        nats.serviceTokenFile,
	}).Env([]string{url})
	if err != nil {
		var input *core.InputError
		if errors.As(err, &input) && input.Input == "socks5Proxy" {
			return nil, core.InputErrorf("natsUrl", "natsUrl names a local NATS server; the pods reach NATS only through the front door's SOCKS5 proxy, so the URL must be remote (the hub's tailnet name)")
		}
		return nil, core.InputErrorf("natsUrl", "%s", strings.ReplaceAll(err.Error(), "natsUrls[0]", "natsUrl"))
	}
	values := append(envVars(env), nats.listenerEnv...)
	values = append(values, secretEnv("ENVOY_API_TOKEN", listenerSecret, "ENVOY_API_TOKEN"))
	return values, nil
}

// dispatchServiceAccountSubject is the subject of the app's projected service-account tokens,
// system:serviceaccount:<namespace>:<hostname>: ServiceAccount <hostname>, which only the app's
// pods run as. The serviceAccountSubject output names it, and so does the listener's
// ENVOY_DISPATCH_PRINCIPAL.
func dispatchServiceAccountSubject(s dispatchSettings) string {
	return "system:serviceaccount:" + s.namespace + ":" + s.hostname
}

// dispatchListenerURL is the listener's own Service address, the one URL every caller reaches it
// at: the app's ENVOY_URL, and the audience of the app's projected token for it, which the
// listener verifies as its ENVOY_OIDC_AUDIENCE.
func dispatchListenerURL(s dispatchSettings) string {
	return "http://" + clusterLocal(s.hostname+"-listener", s.namespace) + ":" + strconv.Itoa(core.DefaultListenerPort)
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

// dispatchAccountTokenVolume is envoy-dispatch's mount and the pod's volume of the app's projected
// token for audience, the listener's URL: minted by the kubelet for dispatchAccountTokenSeconds
// into dispatchAccountTokenDir, read-only, which ENVOY_ACCOUNT_TOKEN_FILE names.
func dispatchAccountTokenVolume(audience string) (corev1.VolumeMountArgs, corev1.VolumeArgs) {
	const name = "envoy-listener-account-token"
	return corev1.VolumeMountArgs{Name: pulumi.String(name), MountPath: pulumi.String(dispatchAccountTokenDir), ReadOnly: pulumi.Bool(true)},
		corev1.VolumeArgs{Name: pulumi.String(name), Projected: &corev1.ProjectedVolumeSourceArgs{Sources: corev1.VolumeProjectionArray{
			corev1.VolumeProjectionArgs{ServiceAccountToken: &corev1.ServiceAccountTokenProjectionArgs{Audience: pulumi.String(audience), ExpirationSeconds: pulumi.Int(dispatchAccountTokenSeconds), Path: pulumi.String("token")}},
		}}}
}
