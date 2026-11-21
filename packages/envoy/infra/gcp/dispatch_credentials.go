package gcp

import (
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// serviceTokenSeconds is the lifetime of each projected ServiceAccount token the app presents to
// the secrets broker and each pod presents to the NATS auth service with natsAudience. Both refuse
// a token living longer than an hour from its issue, so the kubelet mints each for exactly that and
// rewrites it at 80% of that life, 48 minutes, ahead of the 50 minutes at which the bus moves to a
// later token (natscred.ServiceRenewBefore).
const serviceTokenSeconds = 3600

// resolveDispatchNATSCredential holds the inputs that choose the pods' NATS credential, before
// anything registers: natsAudience, an audience no whitespace or control character may sit in, or
// else natsSeedSecret, and never both, since each connection presents one credential and the bus
// refuses a token beside a seed when it starts. Each refusal names its input.
func resolveDispatchNATSCredential(args *DispatchArgs) error {
	if args.NatsAudience == "" {
		if args.NatsSeedSecret == nil {
			return core.InputErrorf("natsSeedSecret", "natsSeedSecret is required unless natsAudience is set")
		}
		return nil
	}
	if err := core.CheckEnvValue("natsAudience", "a projected ServiceAccount token's audience", args.NatsAudience); err != nil {
		return err
	}
	if args.NatsSeedSecret != nil {
		return core.InputErrorf("natsSeedSecret", "natsSeedSecret is set beside natsAudience: each NATS connection presents one credential, which natsAudience makes its pod's projected ServiceAccount token, so unset natsSeedSecret")
	}
	return nil
}

// Where the pods' NATS credential is mounted, each volume named for its one file. Both Envoy
// containers mount it at dispatchCredentialMount: the seed, the one file of its
// SecretProviderClass, which NATS_NKEY_SEED_FILE names, or, with natsAudience, the projected token
// of the pod's own ServiceAccount for it, which NATS_SERVICE_TOKEN_FILE names.
const (
	dispatchCredentialMount = "/var/run/secrets/envoy"
	dispatchNatsSeedName    = "nats-seed"
	dispatchNatsSeedFile    = dispatchCredentialMount + "/" + dispatchNatsSeedName
	dispatchNatsTokenName   = "nats-token"
	dispatchNatsTokenFile   = dispatchCredentialMount + "/" + dispatchNatsTokenName
)

// projectedTokenVolume is a volume holding one file, name, the pod's own ServiceAccount token for
// audience, which the kubelet mints for serviceTokenSeconds and rewrites in place before it
// expires, so nothing stores the token and no rotation restarts a pod.
func projectedTokenVolume(name, audience string) corev1.VolumeArgs {
	return corev1.VolumeArgs{Name: pulumi.String(name), Projected: &corev1.ProjectedVolumeSourceArgs{Sources: corev1.VolumeProjectionArray{
		corev1.VolumeProjectionArgs{ServiceAccountToken: &corev1.ServiceAccountTokenProjectionArgs{Audience: pulumi.String(audience), ExpirationSeconds: pulumi.Int(serviceTokenSeconds), Path: pulumi.String(name)}},
	}}}
}

// dispatchNATSCredential is the NATS credential the app's envoy-dispatch and the listener's
// envoy-listener connect with, prepared once (newDispatchNATSCredential) and consumed whole by
// both pods: each mounts volume at mount, and each names the file in its environment.
type dispatchNATSCredential struct {
	// serviceTokenFile is the listener's core.ListenerConfig.ServiceTokenFile, which core's Env
	// renders as NATS_SERVICE_TOKEN_FILE, and listenerEnv what follows core's rendering in the
	// listener's environment. appEnv is the app's one variable, in the seed's slot of its list.
	serviceTokenFile string
	listenerEnv      corev1.EnvVarArray
	appEnv           corev1.EnvVarArgs
	volume           corev1.VolumeArgs
	mount            corev1.VolumeMountArgs
	// appGrants and listenerGrants are what each Deployment waits for before its pods can read the
	// credential: the seed's grant to that pod's own account.
	appGrants, listenerGrants []pulumi.Resource
}

// newDispatchNATSCredential prepares the pods' NATS credential. Without natsAudience it is the
// seed: a SecretProviderClass of natsSeedSecret (secretClass), mounted through the CSI driver,
// which a pod can read only once its own account's principal holds
// roles/secretmanager.secretAccessor on the secret. newDispatchDatabase grants it to the app's
// account and to the listener's, and each Deployment waits for its own account's grant rather
// than racing IAM on the first deployment. With natsAudience it is each pod's projected token of
// its own ServiceAccount for that audience, the app's <hostname> and the listener's
// <hostname>-listener, which needs no class and no grant. A deployment without natsAudience
// renders what it always rendered.
func newDispatchNATSCredential(s dispatchSettings, args *DispatchArgs, database *dispatchDatabase, secretClass func(name string, secret pulumi.StringInput, input, file string) error) (dispatchNATSCredential, error) {
	if s.natsAudience != "" {
		return dispatchNATSCredential{
			serviceTokenFile: dispatchNatsTokenFile,
			appEnv:           valueEnv(bus.ServiceTokenFileEnvVar, dispatchNatsTokenFile),
			volume:           projectedTokenVolume(dispatchNatsTokenName, s.natsAudience),
			mount:            credentialMount(dispatchNatsTokenName),
		}, nil
	}
	seedName := s.hostname + "-nats-seed"
	if err := secretClass(seedName, args.NatsSeedSecret, "natsSeedSecret", dispatchNatsSeedName); err != nil {
		return dispatchNATSCredential{}, err
	}
	return dispatchNATSCredential{
		listenerEnv:    corev1.EnvVarArray{valueEnv("NATS_NKEY_SEED_FILE", dispatchNatsSeedFile)},
		appEnv:         valueEnv("NATS_NKEY_SEED_FILE", dispatchNatsSeedFile),
		volume:         corev1.VolumeArgs{Name: pulumi.String(dispatchNatsSeedName), Csi: &corev1.CSIVolumeSourceArgs{Driver: pulumi.String("secrets-store-gke.csi.k8s.io"), ReadOnly: pulumi.Bool(true), VolumeAttributes: pulumi.StringMap{"secretProviderClass": pulumi.String(seedName)}}},
		mount:          credentialMount(dispatchNatsSeedName),
		appGrants:      []pulumi.Resource{database.seedAccessor},
		listenerGrants: []pulumi.Resource{database.listenerSeedAccessor},
	}, nil
}

// credentialMount mounts the NATS credential's volume, name, read-only at
// dispatchCredentialMount.
func credentialMount(name string) corev1.VolumeMountArgs {
	return corev1.VolumeMountArgs{Name: pulumi.String(name), MountPath: pulumi.String(dispatchCredentialMount), ReadOnly: pulumi.Bool(true)}
}
