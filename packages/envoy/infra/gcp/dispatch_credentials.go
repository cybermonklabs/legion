package gcp

// The NATS credential the app's envoy-dispatch and the listener's envoy-listener connect with, one
// for both pods: the seed natsSeedSecret holds, or, with natsAudience, each pod's projected
// ServiceAccount token for that audience (newDispatchNATSCredential), and the refusals that hold
// the two inputs to exactly one of them (refuseDispatchNATSCredential).

import (
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
)

// refuseDispatchNATSCredential holds natsSeedSecret and natsAudience, the two inputs that choose
// the pods' NATS credential, to exactly one of them before anything registers, each refusal a
// core.InputError: natsAudience beside natsSeedSecret is refused naming natsAudience, since each
// connection presents one credential and the bus refuses a token beside a seed when it starts;
// neither of them is refused naming natsSeedSecret, which is required unless natsAudience is set;
// and natsAudience holding whitespace or a control character is refused naming it, since the
// broker reads its own audience, BROKER_NATS_K8S_AUDIENCE, trimmed, so a token minted for that
// audience would never verify. A seed in natsAudience is dispatchSeededInput's to refuse, before
// any of these, and natsAudience equal to another audience of the component is
// refuseSharedAudiences'.
func refuseDispatchNATSCredential(args *DispatchArgs) error {
	if args.NatsAudience == "" {
		if args.NatsSeedSecret == nil {
			return core.InputErrorf("natsSeedSecret", "natsSeedSecret is required unless natsAudience is set")
		}
		return nil
	}
	if args.NatsSeedSecret != nil {
		return core.InputErrorf("natsAudience", "natsAudience is set beside natsSeedSecret: the app and the listener each connect to NATS as one identity, "+
			"the projected ServiceAccount token for natsAudience or the seed's user, and with natsAudience nothing of the seed is mounted or granted; unset natsSeedSecret, or natsAudience")
	}
	if holdsSpaceOrControl(args.NatsAudience) {
		return natsAudienceRefused("natsAudience", args.NatsAudience)
	}
	return nil
}

// dispatchNATSCredential is the NATS credential both Envoy containers connect with, prepared once
// (newDispatchNATSCredential) and taken whole by both pods: env is the one variable naming its
// file, which each container's environment holds in the place the seed's always held, volume the
// pod's volume holding that file, and mount the container's read-only mount of it at
// natsCredentialDir (natsCredentialMount). appGrants and listenerGrants are what the app's and the
// listener's Deployments each wait for before their pods can read the credential, nil where
// nothing is granted.
type dispatchNATSCredential struct {
	env                       corev1.EnvVarArgs
	volume                    corev1.VolumeArgs
	mount                     corev1.VolumeMountArgs
	appGrants, listenerGrants []pulumi.Resource
}

// newDispatchNATSCredential prepares the pods' NATS credential once both ServiceAccounts are
// registered: the app's, <hostname>, whose uid is appUID, and listenerAccount, <hostname>-listener.
//
// Without natsAudience it is the seed, as it always was: SecretProviderClass <hostname>-nats-seed
// of natsSeedSecret (secretClass), mounted through the CSI driver as the file NATS_NKEY_SEED_FILE
// names, which a pod reads only once its own account's principal holds
// roles/secretmanager.secretAccessor on the secret. Each of the two accounts holds it by name
// (newDispatchDatabase's two grants) and by uid (newDispatchSeedGrantByUID, registered here from
// each account's uid), and each Deployment waits for its own account's two grants rather than
// racing IAM on the first deployment.
//
// With natsAudience it is each pod's projected token of its own ServiceAccount for that audience,
// the app's <hostname> and the listener's <hostname>-listener, which the kubelet mints into the pod
// and rewrites in place before it lapses, and which NATS_SERVICE_TOKEN_FILE names: each pod
// presents it to the NATS auth service as user "service" (internal/bus, credential), and both
// connect as Dispatch of machine id <hostname>, which the broker's BROKER_NATS_SERVICES admits by
// each account. Nothing of the seed is registered, no SecretProviderClass and no grant by name or
// by uid, so neither account can read a seed and no Deployment waits on a grant; the listener's
// uid, which only its grant by uid reads, is not read at all.
func newDispatchNATSCredential(ctx *pulumi.Context, comp *Dispatch, name string, args *DispatchArgs, s dispatchSettings, cloud *dispatchCluster, database *dispatchDatabase,
	appUID pulumi.StringOutput, listenerAccount *corev1.ServiceAccount, secretClass func(name string, secret pulumi.StringInput, input, file string) error,
) (dispatchNATSCredential, error) {
	var nats dispatchNATSCredential
	if s.natsAudience != "" {
		nats.env, nats.volume = natsTokenCredential(s.natsAudience)
	} else {
		dispatch := dispatchType + " " + name
		listenerName := s.hostname + "-listener"
		appGrantByUID, err := newDispatchSeedGrantByUID(ctx, comp, name, args, cloud, database, s.hostname, appUID)
		if err != nil {
			return dispatchNATSCredential{}, err
		}
		listenerGrantByUID, err := newDispatchSeedGrantByUID(ctx, comp, name, args, cloud, database, listenerName, dispatchServiceAccountUID(ctx, dispatch, listenerName, listenerAccount))
		if err != nil {
			return dispatchNATSCredential{}, err
		}
		className := s.hostname + "-nats-seed"
		if err := secretClass(className, args.NatsSeedSecret, "natsSeedSecret", natsSeedFile); err != nil {
			return dispatchNATSCredential{}, err
		}
		nats.env = valueEnv("NATS_NKEY_SEED_FILE", natsCredentialDir+"/"+natsSeedFile)
		nats.volume = corev1.VolumeArgs{Name: pulumi.String(natsSeedFile), Csi: &corev1.CSIVolumeSourceArgs{
			Driver: pulumi.String("secrets-store-gke.csi.k8s.io"), ReadOnly: pulumi.Bool(true), VolumeAttributes: pulumi.StringMap{"secretProviderClass": pulumi.String(className)},
		}}
		nats.appGrants = []pulumi.Resource{database.seedAccessor, appGrantByUID}
		nats.listenerGrants = []pulumi.Resource{database.listenerSeedAccessor, listenerGrantByUID}
	}
	nats.mount = natsCredentialMount(nats.volume)
	return nats, nil
}
