package gcp

// The NATS credential an Envoy container connects with, the same for gcp:ClusteredNatsHub's
// session listener and gcp:Dispatch's app and listener: one file in one volume of its pod, mounted
// read-only at natsCredentialDir (natsCredentialMount), the seed's in the seeded mode and, with
// natsAudience, a projected token of the pod's own ServiceAccount (natsTokenCredential); and the
// refusal of a natsAudience the broker's own audience can never equal (natsAudienceRefused).

import (
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// Where an Envoy container mounts its pod's NATS credential, read-only, its volume named for the
// one file it holds: the seed, which NATS_NKEY_SEED_FILE names, the one file of the hub listener's
// operator Secret or of Dispatch's SecretProviderClass <hostname>-nats-seed, or, with
// natsAudience, the projected token of the pod's own ServiceAccount, which NATS_SERVICE_TOKEN_FILE
// (bus.ServiceTokenFileEnvVar) names. natsTokenSeconds is that token's expirationSeconds: the
// kubelet rewrites the file at 80% of that life, 48 minutes, ahead of the 50 minutes at which the
// bus moves to a later token (natscred.ServiceRenewBefore, ten minutes before the token it
// connected with lapses), and the auth service admits a platform token that lives at most an hour
// and its clock skew past its issue (natscallout's longestServiceToken).
const (
	natsCredentialDir = "/var/run/secrets/envoy"
	natsSeedFile      = "nats-seed"
	natsTokenFile     = "nats-token"
	natsTokenSeconds  = 3600
)

// natsTokenCredential is the token mode's NATS credential for audience: the one variable naming
// its file, NATS_SERVICE_TOKEN_FILE, whose token the container presents to the NATS auth service
// as user "service" (internal/bus, credential), and the pod's volume holding it, the projected
// token of the pod's own ServiceAccount for audience, which the kubelet mints into the pod and
// rewrites in place before it lapses.
func natsTokenCredential(audience string) (corev1.EnvVarArgs, corev1.VolumeArgs) {
	return valueEnv(bus.ServiceTokenFileEnvVar, natsCredentialDir+"/"+natsTokenFile),
		corev1.VolumeArgs{Name: pulumi.String(natsTokenFile), Projected: &corev1.ProjectedVolumeSourceArgs{Sources: corev1.VolumeProjectionArray{
			corev1.VolumeProjectionArgs{ServiceAccountToken: &corev1.ServiceAccountTokenProjectionArgs{
				Audience: pulumi.String(audience), ExpirationSeconds: pulumi.Int(natsTokenSeconds), Path: pulumi.String(natsTokenFile),
			}},
		}}}
}

// natsCredentialMount is a container's read-only mount of volume, its pod's NATS credential, at
// natsCredentialDir.
func natsCredentialMount(volume corev1.VolumeArgs) corev1.VolumeMountArgs {
	return corev1.VolumeMountArgs{Name: volume.Name, MountPath: pulumi.String(natsCredentialDir), ReadOnly: pulumi.Bool(true)}
}

// natsAudienceRefused is the refusal of audience, the natsAudience that input names, for holding
// whitespace or a control character (holdsSpaceOrControl): the broker reads its own audience
// trimmed, so a token minted for such an audience would never verify. It is a core.InputError
// naming input: <path>.natsAudience on the hub's listener, natsAudience on gcp:Dispatch.
func natsAudienceRefused(input, audience string) error {
	return core.InputErrorf(input, "%s %q holds whitespace or a control character: the broker reads its own audience, BROKER_NATS_K8S_AUDIENCE, trimmed, "+
		"so a token minted for this one would never verify", input, audience)
}
